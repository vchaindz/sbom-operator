package daemon

import (
	"time"

	"github.com/ckotzbauer/sbom-operator/internal"
	"github.com/ckotzbauer/sbom-operator/internal/job"
	"github.com/ckotzbauer/sbom-operator/internal/kubernetes"
	"github.com/ckotzbauer/sbom-operator/internal/syft"
	"github.com/ckotzbauer/sbom-operator/internal/target"
	"github.com/robfig/cron"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

type CronService struct {
	cron    string
	targets []target.Target
}

var running = false

func Start(cronTime string) {
	cr := internal.Unescape(cronTime)
	targetKeys := viper.GetStringSlice(internal.ConfigKeyTargets)

	logrus.Debugf("Cron set to: %v", cr)
	targets := make([]target.Target, 0)

	if !hasJobImage() {
		logrus.Debugf("Targets set to: %v", targetKeys)
		targets = initTargets(targetKeys)
	}

	cs := CronService{cron: cr, targets: targets}
	cs.printNextExecution()

	c := cron.New()
	c.AddFunc(cr, func() { cs.runBackgroundService() })
	c.Start()
}

func (c *CronService) printNextExecution() {
	s, err := cron.Parse(c.cron)
	if err != nil {
		logrus.WithError(err).Fatal("Cron cannot be parsed")
	}

	nextRun := s.Next(time.Now())
	logrus.Debugf("Next background-service run at: %v", nextRun)
}

func (c *CronService) runBackgroundService() {
	if running {
		return
	}

	running = true

	logrus.Info("Execute background-service")
	format := viper.GetString(internal.ConfigKeyFormat)

	if !hasJobImage() {
		for _, t := range c.targets {
			t.Initialize()
		}
	}

	k8s := kubernetes.NewClient()
	namespaces := k8s.ListNamespaces(viper.GetString(internal.ConfigKeyNamespaceLabelSelector))
	logrus.Debugf("Discovered %v namespaces", len(namespaces))
	containerImages, allImages := k8s.LoadImageInfos(namespaces, viper.GetString(internal.ConfigKeyPodLabelSelector))

	if !hasJobImage() {
		c.executeSyftScans(format, k8s, containerImages, allImages)
	} else {
		executeJobImage(k8s, containerImages)
	}

	c.printNextExecution()
	running = false
}

func (c *CronService) executeSyftScans(format string, k8s *kubernetes.KubeClient,
	containerImages map[string]kubernetes.ContainerImage, allImages []kubernetes.ContainerImage) {
	sy := syft.New(format)

	for _, image := range containerImages {
		sbom, err := sy.ExecuteSyft(image)
		if err != nil {
			// Error is already handled from syft module.
			continue
		}

		errOccurred := false

		for _, t := range c.targets {
			err = t.ProcessSbom(image, sbom)
			errOccurred = errOccurred || err != nil
		}

		if !errOccurred {
			for _, pod := range image.Pods {
				k8s.UpdatePodAnnotation(pod)
			}
		}
	}

	for _, t := range c.targets {
		t.Cleanup(allImages)
	}
}

func executeJobImage(k8s *kubernetes.KubeClient, containerImages map[string]kubernetes.ContainerImage) {
	j, err := job.StartJob(k8s, containerImages)
	if err != nil {
		// Already handled from job-module
		return
	}

	if job.WaitForJob(k8s, j) {
		for _, i := range containerImages {
			for _, pod := range i.Pods {
				k8s.UpdatePodAnnotation(pod)
			}
		}
	}
}

func initTargets(targetKeys []string) []target.Target {
	targets := make([]target.Target, 0)

	for _, ta := range targetKeys {
		var err error

		if ta == "git" {
			t := target.NewGitTarget()
			err = t.ValidateConfig()
			targets = append(targets, t)
		} else if ta == "dtrack" {
			t := target.NewDependencyTrackTarget()
			err = t.ValidateConfig()
			targets = append(targets, t)
		} else {
			logrus.Fatalf("Unknown target %s", ta)
		}

		if err != nil {
			logrus.WithError(err).Fatal("Config-Validation failed!")
		}
	}

	if len(targets) == 0 {
		logrus.Fatalf("Please specify at least one target.")
	}

	return targets
}

func hasJobImage() bool {
	return viper.GetString(internal.ConfigKeyJobImage) != ""
}
