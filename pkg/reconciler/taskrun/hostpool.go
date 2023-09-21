package taskrun

import (
	"context"
	"github.com/go-logr/logr"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	v12 "k8s.io/api/core/v1"
	"k8s.io/utils/strings/slices"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"strings"
	"time"
)

type HostPool struct {
	hosts          map[string]*Host
	targetPlatform string
}

func (hp HostPool) Allocate(r *ReconcileTaskRun, ctx context.Context, log *logr.Logger, tr *v1beta1.TaskRun, secretName string, instanceTag string) (reconcile.Result, error) {
	if len(hp.hosts) == 0 {
		//no hosts configured
		return reconcile.Result{}, r.createErrorSecret(ctx, tr, secretName, "no hosts configured")
	}
	failedString := tr.Annotations[FailedHosts]
	failed := strings.Split(failedString, ",")

	//get all existing runs that are assigned to a host
	taskList := v1beta1.TaskRunList{}
	err := r.client.List(ctx, &taskList, client.HasLabels{AssignedHost})
	if err != nil {
		return reconcile.Result{}, err
	}
	hostCount := map[string]int{}
	for _, tr := range taskList.Items {
		if tr.Labels[TaskTypeLabel] == "" {
			host := tr.Labels[AssignedHost]
			hostCount[host] = hostCount[host] + 1
		}
	}
	for k, v := range hostCount {
		log.Info("host count", "host", k, "count", v)
	}

	//now select the host with the most free spots
	//this algorithm is not very complex

	var selected *Host
	freeSpots := 0
	hostWithOurPlatform := false
	for k, v := range hp.hosts {
		if slices.Contains(failed, k) {
			log.Info("ignoring already failed host", "host", k, "targetPlatform", hp.targetPlatform, "hostPlatform", v.Platform)
			continue
		}
		if v.Platform != hp.targetPlatform {
			log.Info("ignoring host", "host", k, "targetPlatform", hp.targetPlatform, "hostPlatform", v.Platform)
			continue
		}
		hostWithOurPlatform = true
		free := v.Concurrency - hostCount[k]

		log.Info("considering host", "host", k, "freeSlots", free)
		if free > freeSpots {
			selected = v
			freeSpots = free
		}
	}
	if !hostWithOurPlatform {
		log.Info("no hosts with requested platform", "platform", hp.targetPlatform, "failed", failedString)
		return reconcile.Result{}, r.createErrorSecret(ctx, tr, secretName, "no hosts configured for platform "+hp.targetPlatform+", attempted hosts: "+failedString)
	}
	if selected == nil {
		if tr.Labels[WaitingForPlatformLabel] == platformLabel(hp.targetPlatform) {
			//we are already in a waiting state
			return reconcile.Result{}, nil
		}
		log.Info("no host found, waiting for one to become available")
		//no host available
		//add the waiting label
		//TODO: is the requeue actually a good idea?
		//TODO: timeout
		tr.Labels[WaitingForPlatformLabel] = platformLabel(hp.targetPlatform)
		return reconcile.Result{RequeueAfter: time.Minute}, r.client.Update(ctx, tr)
	}

	log.Info("allocated host", "host", selected.Name)
	tr.Labels[AssignedHost] = selected.Name
	delete(tr.Labels, WaitingForPlatformLabel)
	//add a finalizer to clean up the secret
	controllerutil.AddFinalizer(tr, PipelineFinalizer)
	err = r.client.Update(ctx, tr)
	if err != nil {
		return reconcile.Result{}, err
	}

	err = launchProvisioningTask(r, ctx, log, tr, secretName, selected.Secret, selected.Address, selected.User)

	if err != nil {
		//ugh, try and unassign
		delete(tr.Labels, AssignedHost)
		updateErr := r.client.Update(ctx, tr)
		if updateErr != nil {
			log.Error(updateErr, "Could not unassign task after provisioning failure")
			_ = r.createErrorSecret(ctx, tr, secretName, "Could not unassign task after provisioning failure")
		} else {
			log.Error(err, "Failed to provision host from pool")
			_ = r.createErrorSecret(ctx, tr, secretName, "Failed to provision host from pool "+err.Error())
		}
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (hp HostPool) Deallocate(r *ReconcileTaskRun, ctx context.Context, log *logr.Logger, tr *v1beta1.TaskRun, secretName string, selectedHost string) error {
	selected := hp.hosts[selectedHost]
	if selected != nil {
		log.Info("starting cleanup task")
		//kick off the clean task
		//kick off the provisioning task
		provision := v1beta1.TaskRun{}
		provision.GenerateName = "cleanup-task"
		provision.Namespace = r.operatorNamespace
		provision.Labels = map[string]string{TaskTypeLabel: TaskTypeClean, UserTaskName: tr.Name, UserTaskNamespace: tr.Namespace, MultiPlatformLabel: "true"}
		provision.Spec.TaskRef = &v1beta1.TaskRef{Name: "clean-shared-host"}
		provision.Spec.Workspaces = []v1beta1.WorkspaceBinding{{Name: "ssh", Secret: &v12.SecretVolumeSource{SecretName: selected.Secret}}}
		provision.Spec.ServiceAccountName = ServiceAccountName //TODO: special service account for this
		provision.Spec.Params = []v1beta1.Param{
			{
				Name:  "SECRET_NAME",
				Value: *v1beta1.NewStructuredValues(secretName),
			},
			{
				Name:  "TASKRUN_NAME",
				Value: *v1beta1.NewStructuredValues(tr.Name),
			},
			{
				Name:  "NAMESPACE",
				Value: *v1beta1.NewStructuredValues(tr.Namespace),
			},
			{
				Name:  "HOST",
				Value: *v1beta1.NewStructuredValues(selected.Address),
			},
			{
				Name:  "USER",
				Value: *v1beta1.NewStructuredValues(selected.User),
			},
		}
		err := r.client.Create(ctx, &provision)
		return err
	}
	return nil

}
