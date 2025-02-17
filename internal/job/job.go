package job

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ckotzbauer/sbom-operator/internal"
	"github.com/ckotzbauer/sbom-operator/internal/kubernetes"
	"github.com/ckotzbauer/sbom-operator/internal/registry"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

type imagePod struct {
	Pod       string `json:"pod"`
	Namespace string `json:"namespace"`
	Cluster   string `json:"cluster"`
}

type imageConfig struct {
	Host     string     `json:"registry-host"`
	User     string     `json:"registry-user"`
	Password string     `json:"registry-password"`
	Image    string     `json:"image"`
	Pods     []imagePod `json:"pods"`
}

func StartJob(k8s *kubernetes.KubeClient, images map[string]kubernetes.ContainerImage) (*batchv1.Job, error) {
	configs := make([]imageConfig, 0)
	jobImage := viper.GetString(internal.ConfigKeyJobImage)
	jobPullSecret := viper.GetString(internal.ConfigKeyJobImagePullSecret)
	jobTimeout := viper.GetInt64(internal.ConfigKeyJobTimeout)
	podNamespace := os.Getenv("POD_NAMESPACE")

	for _, image := range images {
		cfg, err := registry.ResolveAuthConfig(image)
		if err != nil {
			logrus.WithError(err).Error("Error occurred during auth-resolve")
			return nil, err
		}

		configs = append(configs, imageConfig{
			Host:     cfg.ServerAddress,
			User:     cfg.Username,
			Password: cfg.Password,
			Image:    image.ImageID,
			Pods:     convertPods(image.Pods),
		})
	}

	bytes, err := json.Marshal(configs)
	if err != nil {
		logrus.WithError(err).Error("Error occurred during config-marshal")
		return nil, err
	}

	suffix := generateObjectSuffix()
	err = k8s.CreateJobSecret(podNamespace, suffix, bytes)
	if err != nil {
		logrus.WithError(err).Error("Error occurred during job-secret creation/update")
		return nil, err
	}

	job, err := k8s.CreateJob(podNamespace, suffix, jobImage, jobPullSecret, jobTimeout, getJobEnvs())
	if err != nil {
		logrus.WithError(err).Error("Error occurred during job creation/update")
		return nil, err
	}

	logrus.Infof("Created job %s-%s", kubernetes.JobName, suffix)
	return job, nil
}

func WaitForJob(k8s *kubernetes.KubeClient, job *batchv1.Job) bool {
	for {
		job, err := k8s.Client.BatchV1().Jobs(job.Namespace).Get(context.Background(), job.Name, meta.GetOptions{})
		if err != nil {
			logrus.WithError(err).Warnf("Error while waiting for job %s.", job.Name)
			return false
		}

		pending := job.Status.Active == 0 && job.Status.Succeeded == 0 && job.Status.Failed == 0
		succeeded := job.Status.Active == 0 && job.Status.Succeeded > 0
		failed := job.Status.Active == 0 && job.Status.Failed > 0

		if !pending && succeeded {
			logrus.Infof("Job succeeded %s", job.Name)
			return true
		} else if !pending && failed {
			logrus.Infof("Job failed %s", job.Name)
			return false
		}

		time.Sleep(10 * time.Second)
	}
}

func generateObjectSuffix() string {
	t := time.Now()
	return t.Format("20060102-150405")
}

func getJobEnvs() map[string]string {
	m := make(map[string]string)
	re := regexp.MustCompile(`SBOM_JOB_(?P<Key>[A-Za-z0-9-_\.]*)=(?P<Value>[A-Za-z0-9-_\.=]*)`)

	for _, v := range os.Environ() {
		matches := re.FindStringSubmatch(v)
		if len(matches) > 1 {
			index := re.SubexpIndex("Key")
			key := matches[index]
			index = re.SubexpIndex("Value")
			m[key] = matches[index]
		}
	}

	return m
}

func convertPods(pods []corev1.Pod) []imagePod {
	ips := make([]imagePod, 0)

	for _, p := range pods {
		ips = append(ips, imagePod{
			Pod:       p.Name,
			Namespace: p.Namespace,
			Cluster:   viper.GetString(internal.ConfigKeyKubernetesClusterId),
		})
	}

	return ips
}
