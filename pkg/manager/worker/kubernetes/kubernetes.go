package kubernetes

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	apiv1 "k8s.io/api/core/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"

	"github.com/kubeflow/katib/pkg/api"
	"github.com/kubeflow/katib/pkg/db"
)

const (
	kubeNamespace = "katib"
)

type KubernetesWorkerInterface struct {
	clientset *kubernetes.Clientset
	db        db.VizierDBInterface
}

func NewKubernetesWorkerInterface(db db.VizierDBInterface) (*KubernetesWorkerInterface, error) {
	config, err := restclient.InClusterConfig()
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &KubernetesWorkerInterface{
		clientset: clientset,
		db:        db,
	}, nil
}

// Generate Job Template
func (d *KubernetesWorkerInterface) genJobManifest(wid string, conf *api.WorkerConfig) (*batchv1.Job, error) {
	//construct entry point nad parameter
	template := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{
			Kind: "Job",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: wid,
			Labels: map[string]string{
				"katib-version": "alpha-0.2.0",
				"worker-id":     wid,
			},
		},
		Spec: batchv1.JobSpec{
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"katib-version": "alpha-0.2.0",
						"worker-id":     wid,
					},
				},
				Spec: apiv1.PodSpec{
					SchedulerName: conf.Scheduler,
					Containers: []apiv1.Container{
						{
							Image:           conf.Image,
							Name:            wid,
							Command:         conf.Command,
							ImagePullPolicy: apiv1.PullAlways,
						},
					},
					RestartPolicy: apiv1.RestartPolicyOnFailure,
					ImagePullSecrets: []apiv1.LocalObjectReference{
						apiv1.LocalObjectReference{
							Name: conf.PullSecret,
						},
					},
				},
			},
		},
	}

	// Katib labels
	labels := map[string] string {
		"katib-version": "alpha-0.2.0",
		"worker-id":     wid,
	}

	// If there are custom labels, add it to the list of labels
	if len(conf.Labels) != 0 {
		for k, v := range conf.Labels {
			labels[k] = v
		}
	}
	template.ObjectMeta.Labels = labels
	template.Spec.Template.ObjectMeta.Labels = labels

	if len(conf.Annotations) != 0 {
		template.Spec.Template.ObjectMeta.Annotations = conf.Annotations;
	}

	if len(conf.Tolerations) > 0 {
		tolerations := []apiv1.Toleration{};
		for i := 0; i<len(conf.Tolerations); i++{
			tolerations = append(tolerations, apiv1.Toleration{
				Key: conf.Tolerations[i].Key,
				Operator: apiv1.TolerationOperator(conf.Tolerations[i].Operator),
				Value: conf.Tolerations[i].Value,
				Effect: apiv1.TaintEffect(conf.Tolerations[i].Effect),
			})
		}
		template.Spec.Template.Spec.Tolerations = tolerations;
	}

	// Specified pvc is mounted to both PS and Worker Pods
	if conf.Mount != nil {
		if conf.Mount.Pvc != "" && conf.Mount.Path != "" {
			template.Spec.Template.Spec.Volumes = []apiv1.Volume{
				apiv1.Volume{
					Name: "pvc-mount-point",
					VolumeSource: apiv1.VolumeSource{
						PersistentVolumeClaim: &apiv1.PersistentVolumeClaimVolumeSource{
							ClaimName: conf.Mount.Pvc,
						},
					},
				},
			}
			template.Spec.Template.Spec.Containers[0].VolumeMounts = []apiv1.VolumeMount{
				apiv1.VolumeMount{
					Name:      "pvc-mount-point",
					MountPath: conf.Mount.Path,
				},
			}
		}
	}

	cpuReq, err := resource.ParseQuantity(strconv.Itoa(int(conf.Cpu)))
	if err != nil {
		return nil, err
	}
	memoryReq, err := resource.ParseQuantity(conf.Memory)
	if err != nil {
		return nil, err
	}

	template.Spec.Template.Spec.Containers[0].Resources = apiv1.ResourceRequirements{
			Limits: apiv1.ResourceList{
				"cpu": cpuReq,
				"memory": memoryReq,
		},
	}
	if conf.Gpu > 0 {
		gpuReq, err := resource.ParseQuantity(strconv.Itoa(int(conf.Gpu)))
		if err != nil {
			return nil, err
		}
		template.Spec.Template.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"] = gpuReq
	}
	return template, nil
}

func (d *KubernetesWorkerInterface) StoreWorkerLog(wID string) error {
	pl, _ := d.clientset.CoreV1().Pods(kubeNamespace).List(metav1.ListOptions{LabelSelector: "job-name=" + wID})
	if len(pl.Items) == 0 {
		return errors.New(fmt.Sprintf("No Pods are found in Job %v", wID))
	}

	mt, err := d.db.GetWorkerTimestamp(wID)
	if err != nil {
		return err
	}
	logopt := apiv1.PodLogOptions{Timestamps: true}
	if mt != nil {
		logopt.SinceTime = &metav1.Time{Time: *mt}
	}

	logs, err := d.clientset.CoreV1().Pods(kubeNamespace).GetLogs(pl.Items[0].ObjectMeta.Name, &logopt).Do().Raw()
	if err != nil {
		return err
	}
	if len(logs) == 0 {
		return nil
	}
	err = d.db.StoreWorkerLogs(wID, strings.Split(string(logs), "\n"))
	return err
}

func (d *KubernetesWorkerInterface) IsWorkerComplete(wID string) (bool, error) {
	jcl := d.clientset.BatchV1().Jobs(kubeNamespace)
	ji, err := jcl.Get(wID, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if ji.Status.Succeeded == 0 {
		return false, nil
	}
	pl, _ := d.clientset.CoreV1().Pods(kubeNamespace).List(metav1.ListOptions{LabelSelector: "job-name=" + wID})
	if len(pl.Items) == 0 {
		return false, errors.New(fmt.Sprintf("No Pods are found in Job %v", wID))
	}
	if pl.Items[0].Status.Phase == "Succeeded" {
		return true, nil
	}
	return false, nil
}

func (d *KubernetesWorkerInterface) UpdateWorkerStatus(studyId string) error {
	ws, err := d.db.GetWorkerList(studyId, "")
	if err != nil {
		return err
	}
	for _, w := range ws {
		if w.Status == api.State_PENDING {
			err = d.StoreWorkerLog(w.WorkerId)
			if err == nil {
				err = d.db.UpdateWorker(w.WorkerId, api.State_RUNNING)
				if err != nil {
					log.Printf("Error updating status for %s: %v", w.WorkerId, err)
					return err
				}
			}
		} else if w.Status == api.State_RUNNING {
			c, err := d.IsWorkerComplete(w.WorkerId)
			if err != nil {
				return err
			}
			err = d.StoreWorkerLog(w.WorkerId)
			if err != nil {
				return err
			}
			if c {
				err := d.db.UpdateWorker(w.WorkerId, api.State_COMPLETED)
				if err != nil {
					return err
				}
				jcl := d.clientset.BatchV1().Jobs(kubeNamespace)
				pcl := d.clientset.CoreV1().Pods(kubeNamespace)
				jcl.Delete(w.WorkerId, &metav1.DeleteOptions{})
				pl, _ := pcl.List(metav1.ListOptions{LabelSelector: "job-name=" + w.WorkerId})
				pcl.Delete(pl.Items[0].ObjectMeta.Name, &metav1.DeleteOptions{})
			}
		}
	}
	return nil
}

func (d *KubernetesWorkerInterface) SpawnWorker(wid string, workerConf *api.WorkerConfig) error {
	job, err := d.genJobManifest(wid, workerConf)
	if err != nil {
		return err
	}
	jcl := d.clientset.BatchV1().Jobs(kubeNamespace)
	result, err := jcl.Create(job)
	if err != nil {
		return err
	}
	log.Printf("Created Job %q.", result.GetObjectMeta().GetName())
	return nil
}

func (d *KubernetesWorkerInterface) CleanWorkers(studyId string) error {
	jcl := d.clientset.BatchV1().Jobs(kubeNamespace)
	pcl := d.clientset.CoreV1().Pods(kubeNamespace)
	ws, err := d.db.GetWorkerList(studyId, "")
	if err != nil {
		return err
	}
	for _, w := range ws {
		if w.Status == api.State_RUNNING {
			jcl.Delete(w.WorkerId, &metav1.DeleteOptions{})
			pl, _ := pcl.List(metav1.ListOptions{LabelSelector: "job-name=" + w.WorkerId})
			pcl.Delete(pl.Items[0].ObjectMeta.Name, &metav1.DeleteOptions{})
			err := d.db.UpdateWorker(w.WorkerId, api.State_KILLED)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *KubernetesWorkerInterface) StopWorkers(studyId string, wIDs []string, iscomplete bool) error {
	ws, err := d.db.GetWorkerList(studyId, "")
	if err != nil {
		return err
	}
	jcl := d.clientset.BatchV1().Jobs(kubeNamespace)
	pcl := d.clientset.CoreV1().Pods(kubeNamespace)
	for _, w := range ws {
		for _, wid := range wIDs {
			if w.Status == api.State_RUNNING && w.WorkerId == wid {
				jcl.Delete(wid, &metav1.DeleteOptions{})
				pl, err := pcl.List(metav1.ListOptions{LabelSelector: "job-name=" + wid})
				if err != nil {
					return err
				}
				pcl.Delete(pl.Items[0].ObjectMeta.Name, &metav1.DeleteOptions{})
				if iscomplete {
					err = d.db.UpdateWorker(wid, api.State_COMPLETED)
				} else {
					err = d.db.UpdateWorker(wid, api.State_KILLED)
				}
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}
