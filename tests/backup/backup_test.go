package tests

import (
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/libopenstorage/openstorage/pkg/sched"
	"github.com/portworx/torpedo/drivers/scheduler/spec"
	appsapi "k8s.io/api/apps/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/reporters"
	. "github.com/onsi/gomega"
	api "github.com/portworx/px-backup-api/pkg/apis/v1"
	"github.com/portworx/sched-ops/k8s/core"
	"github.com/portworx/torpedo/drivers/scheduler"
	. "github.com/portworx/torpedo/tests"
	"github.com/sirupsen/logrus"
)

const (
	BLocationName                     = "tp-blocation"
	ClusterName                       = "tp-cluster"
	CredName                          = "tp-backup-cred"
	BackupName                        = "tp-backup"
	RestoreName                       = "tp-restore"
	BackupRestoreCompletionTimeoutMin = 3
	RetrySeconds                      = 30
	ConfigMapName                     = "kubeconfigs"
	KubeconfigDirectory               = "/tmp"
	SourceClusterName                 = "source-cluster"
	DestinationClusterName            = "destination-cluster"

	storkDeploymentName      = "stork"
	storkDeploymentNamespace = "kube-system"

	providerAws   = "aws"
	providerAzure = "azure"
	providerGke   = "gke"

	triggerCheckInterval = 2 * time.Second
	triggerCheckTimeout  = 30 * time.Minute

	defaultTimeout       = 5 * time.Minute
	defaultRetryInterval = 10 * time.Second
)

var orgID string = "tp-org"

func TestBackup(t *testing.T) {
	RegisterFailHandler(Fail)

	var specReporters []Reporter
	junitReporter := reporters.NewJUnitReporter("/testresults/junit_basic.xml")
	specReporters = append(specReporters, junitReporter)
	RunSpecsWithDefaultAndCustomReporters(t, "Torpedo : Backup", specReporters)
}

func SetupBackup() {
	orgID = Inst().InstanceID

	provider := os.Getenv("PROVIDER")

	if provider == "" {
		logrus.Warnf("Empty provider")
		return
	}

	CreateOrganization(orgID)
	CreateCloudCredential(provider, CredName, orgID)
	CreateBackupLocation(provider, BLocationName, CredName, orgID)
	CreateSourceAndDestClusters(provider, CredName, orgID)
}

func BackupCleanup() {
	DeleteRestore(RestoreName, orgID)
	DeleteBackup(BackupName, SourceClusterName, orgID)
	DeleteCluster(DestinationClusterName, orgID)
	DeleteCluster(SourceClusterName, orgID)
	DeleteBackupLocation(BLocationName, orgID)
	DeleteCloudCredential(CredName, orgID)
}

var _ = AfterSuite(func() {
	//PerformSystemCheck()
	//ValidateCleanup()
	//	BackupCleanup()
})

func TestMain(m *testing.M) {
	// call flag.Parse() here if TestMain uses flags
	ParseFlags()
	os.Exit(m.Run())
}

func SetClusterContext(clusterConfigPath string) {
	err := Inst().S.SetConfig(clusterConfigPath)
	Expect(err).NotTo(HaveOccurred(),
		fmt.Sprintf("Failed to switch to context. Error: [%v]", err))

	err = Inst().S.RefreshNodeRegistry()
	Expect(err).NotTo(HaveOccurred())

	err = Inst().V.RefreshDriverEndpoints()
	Expect(err).NotTo(HaveOccurred())
}

// This test performs basic test of starting an application, backing it up and killing stork while
// performing backup.
var _ = Describe("{BackupCreateKillStoreRestore}", func() {
	var contexts []*scheduler.Context
	var bkpNamespaces []string

	namespaceMapping := make(map[string]string)
	labelSelectores := make(map[string]string)

	It("has to connect and check the backup setup", func() {
		SetupBackup()

		sourceClusterConfigPath, err := getSourceClusterConfigPath()
		Expect(err).NotTo(HaveOccurred(),
			fmt.Sprintf("Failed to get kubeconfig path for source cluster. Error: [%v]", err))

		SetClusterContext(sourceClusterConfigPath)

		Step("Deploy applications", func() {
			contexts = make([]*scheduler.Context, 0)
			bkpNamespaces = make([]string, 0)
			for i := 0; i < Inst().ScaleFactor; i++ {
				taskName := fmt.Sprintf("backupcreaterestore-%d", i)
				appContexts := ScheduleApplications(taskName)
				contexts = append(contexts, appContexts...)
				for _, ctx := range appContexts {
					namespace := GetAppNamespace(ctx, taskName)
					bkpNamespaces = append(bkpNamespaces, namespace)
					namespaceMapping[namespace] = fmt.Sprintf("%s-restore", namespace)
				}
			}
			ValidateApplications(contexts)
		})

		// Wait for IO to run
		time.Sleep(time.Minute * 20)

		Step(fmt.Sprintf("Create Backup [%s]", BackupName), func() {
			// TODO(stgleb): Add multi-namespace backup when ready in px-backup
			for _, namespace := range bkpNamespaces {
				CreateBackup(fmt.Sprintf("%s-%s", BackupName, namespace),
					SourceClusterName, BLocationName,
					[]string{namespace}, labelSelectores, orgID)
			}
		})

		Step("Kill stork", func() {
			// setup task to delete stork pods as soon as it starts doing backup
			eventCheck := func() (bool, error) {
				for _, namespace := range bkpNamespaces {
					req := &api.BackupInspectRequest{
						Name:  fmt.Sprintf("%s-%s", BackupName, namespace),
						OrgId: orgID,
					}

					err := Inst().Backup.WaitForRunning(req, time.Millisecond, time.Millisecond)

					if err != nil {
						continue
					} else {
						return true, nil
					}
				}
				return false, nil
			}

			deleteOpts := &scheduler.DeleteTasksOptions{
				TriggerOptions: scheduler.TriggerOptions{
					TriggerCb:            eventCheck,
					TriggerCheckInterval: triggerCheckInterval,
					TriggerCheckTimeout:  triggerCheckTimeout,
				},
			}

			t := func(interval sched.Interval) {
				ctx := &scheduler.Context{
					App: &spec.AppSpec{
						SpecList: []interface{}{
							&appsapi.Deployment{
								ObjectMeta: meta_v1.ObjectMeta{
									Name:      storkDeploymentName,
									Namespace: storkDeploymentNamespace,
								},
							},
						},
					},
				}

				err := Inst().S.DeleteTasks(ctx, deleteOpts)
				Expect(err).NotTo(HaveOccurred())
			}

			taskID, err := sched.Instance().Schedule(t,
				sched.Periodic(time.Second),
				time.Now(), true)
			Expect(err).NotTo(HaveOccurred())
			defer sched.Instance().Cancel(taskID)
		})

		Step(fmt.Sprintf("Wait for Backup [%s] to complete", BackupName), func() {
			for _, namespace := range bkpNamespaces {
				err := Inst().Backup.WaitForBackupCompletion(
					fmt.Sprintf("%s-%s", BackupName, namespace), orgID,
					BackupRestoreCompletionTimeoutMin*time.Minute,
					RetrySeconds*time.Second)
				Expect(err).NotTo(HaveOccurred(),
					fmt.Sprintf("Failed to wait for backup [%s] to complete. Error: [%v]",
						BackupName, err))
			}
		})

		Step(fmt.Sprintf("Create Restore [%s]", RestoreName), func() {
			for _, namespace := range bkpNamespaces {
				CreateRestore(fmt.Sprintf("%s-%s", RestoreName, namespace),
					fmt.Sprintf("%s-%s", BackupName, namespace),
					namespaceMapping, DestinationClusterName, orgID)
			}
		})

		// Change namespaces to restored apps only after backed up apps are cleaned up
		// to avoid switching back namespaces to backup namespaces
		Step(fmt.Sprintf("Validate Restore [%s]", RestoreName), func() {
			destClusterConfigPath, err := getDestinationClusterConfigPath()
			Expect(err).NotTo(HaveOccurred(),
				fmt.Sprintf("Failed to get kubeconfig path for destination cluster. Error: [%v]", err))

			err = Inst().S.SetConfig(destClusterConfigPath)
			Expect(err).NotTo(HaveOccurred(),
				fmt.Sprintf("Failed to switch to context to destination cluster. Error: [%v]", err))

			ValidateApplications(contexts)
		})

		Step("teardown all restored apps", func() {
			for _, ctx := range contexts {
				TearDownContext(ctx, nil)
			}
		})

		Step("teardown backup objects", func() {
			BackupCleanup()
		})
	})
})

// This test performs basic test of starting an application and destroying it (along with storage)
var _ = Describe("{BackupCreateRestore}", func() {
	var contexts []*scheduler.Context
	var bkpNamespaces []string
	var namespaceMapping map[string]string
	labelSelectores := make(map[string]string)

	It("has to complete backup and restore", func() {

		SetupBackup()

		sourceClusterConfigPath, err := getSourceClusterConfigPath()
		Expect(err).NotTo(HaveOccurred(),
			fmt.Sprintf("Failed to get kubeconfig path for source cluster. Error: [%v]", err))

		SetClusterContext(sourceClusterConfigPath)

		Step("Deploy applications", func() {
			contexts = make([]*scheduler.Context, 0)
			bkpNamespaces = make([]string, 0)
			for i := 0; i < Inst().ScaleFactor; i++ {
				taskName := fmt.Sprintf("backupcreaterestore-%d", i)
				appContexts := ScheduleApplications(taskName)
				contexts = append(contexts, appContexts...)
				for _, ctx := range appContexts {
					bkpNamespaces = append(bkpNamespaces, GetAppNamespace(ctx, taskName))
				}
			}

			ValidateApplications(contexts)
		})

		Step(fmt.Sprintf("Create Backup [%s]", BackupName), func() {
			CreateBackup(BackupName, SourceClusterName, BLocationName,
				bkpNamespaces, labelSelectores, orgID)
		})

		Step(fmt.Sprintf("Wait for Backup [%s] to complete", BackupName), func() {
			err := Inst().Backup.WaitForBackupCompletion(BackupName, orgID,
				BackupRestoreCompletionTimeoutMin*time.Minute,
				RetrySeconds*time.Second)
			Expect(err).NotTo(HaveOccurred(),
				fmt.Sprintf("Failed to wait for backup [%s] to complete. Error: [%v]",
					BackupName, err))
		})

		Step(fmt.Sprintf("Create Restore [%s]", RestoreName), func() {
			CreateRestore(RestoreName, BackupName,
				namespaceMapping, DestinationClusterName, orgID)
		})

		Step(fmt.Sprintf("Wait for Restore [%s] to complete", RestoreName), func() {
			err := Inst().Backup.WaitForRestoreCompletion(RestoreName, orgID,
				BackupRestoreCompletionTimeoutMin*time.Minute,
				RetrySeconds*time.Second)
			Expect(err).NotTo(HaveOccurred(),
				fmt.Sprintf("Failed to wait for restore [%s] to complete. Error: [%v]",
					BackupName, err))
		})

		Step("teardown all applications on source cluster before switching context to destination cluster", func() {
			for _, ctx := range contexts {
				TearDownContext(ctx, map[string]bool{
					SkipClusterScopedObjects: true,
				})
			}
		})

		// Change namespaces to restored apps only after backed up apps are cleaned up
		// to avoid switching back namespaces to backup namespaces
		Step(fmt.Sprintf("Validate Restore [%s]", RestoreName), func() {
			destClusterConfigPath, err := getDestinationClusterConfigPath()
			Expect(err).NotTo(HaveOccurred(),
				fmt.Sprintf("Failed to get kubeconfig path for destination cluster. Error: [%v]", err))

			SetClusterContext(destClusterConfigPath)

			for _, ctx := range contexts {
				err = Inst().S.WaitForRunning(ctx, defaultTimeout, defaultRetryInterval)
				Expect(err).NotTo(HaveOccurred())
			}
			// TODO: Restored PVCs are created by stork-snapshot StorageClass
			// And not by respective app's StorageClass. Need to fix below function
			// ValidateApplications(contexts)
		})

		Step("teardown all restored apps", func() {
			for _, ctx := range contexts {
				TearDownContext(ctx, nil)
			}
		})

		Step("teardown backup objects", func() {
			BackupCleanup()
		})
	})
})

// CreateOrganization creates org on px-backup
func CreateOrganization(orgID string) {
	Step(fmt.Sprintf("Create organization [%s]", orgID), func() {
		backupDriver := Inst().Backup
		req := &api.OrganizationCreateRequest{
			CreateMetadata: &api.CreateMetadata{
				Name: orgID,
			},
		}
		_, err := backupDriver.CreateOrganization(req)
		Expect(err).NotTo(HaveOccurred(),
			fmt.Sprintf("Failed to create organization [%s]", orgID))
	})
}

// TODO: There is no delete org API
/*func DeleteOrganization(orgID string) {
	Step(fmt.Sprintf("Delete organization [%s]", orgID), func() {
		backupDriver := Inst().Backup
		req := &api.Delete{
			CreateMetadata: &api.CreateMetadata{
				Name: orgID,
			},
		}
		_, err := backupDriver.Delete(req)
		Expect(err).NotTo(HaveOccurred())
	})
}*/

// DeleteCloudCredential deletes cloud credentials
func DeleteCloudCredential(name string, orgID string) {
	Step(fmt.Sprintf("Delete cloud credential [%s] in org [%s]", name, orgID), func() {
		backupDriver := Inst().Backup

		credDeleteRequest := &api.CloudCredentialDeleteRequest{
			Name:  name,
			OrgId: orgID,
		}
		backupDriver.DeleteCloudCredential(credDeleteRequest)
		// Best effort cleanup, dont fail test, if deletion fails
		// Expect(err).NotTo(HaveOccurred(),
		//	fmt.Sprintf("Failed to delete cloud credential [%s] in org [%s]", name, orgID))
		// TODO: validate CreateCloudCredentialResponse also
	})

}

// CreateCloudCredential creates cloud credentials
func CreateCloudCredential(provider, name string, orgID string) {

	Step(fmt.Sprintf("Create cloud credential [%s] in org [%s]", name, orgID), func() {
		backupDriver := Inst().Backup
		switch provider {
		case providerAws:
			// TODO: add separate function to return cred object based on type
			id := os.Getenv("AWS_ACCESS_KEY_ID")
			Expect(id).NotTo(Equal(""),
				"AWS_ACCESS_KEY_ID Environment variable should not be empty")

			secret := os.Getenv("AWS_SECRET_ACCESS_KEY")
			Expect(secret).NotTo(Equal(""),
				"AWS_SECRET_ACCESS_KEY Environment variable should not be empty")

			credCreateRequest := &api.CloudCredentialCreateRequest{
				CreateMetadata: &api.CreateMetadata{
					Name:  name,
					OrgId: orgID,
				},
				CloudCredential: &api.CloudCredentialInfo{
					Type: api.CloudCredentialInfo_AWS,
					Config: &api.CloudCredentialInfo_AwsConfig{
						AwsConfig: &api.AWSConfig{
							AccessKey: id,
							SecretKey: secret,
						},
					},
				},
			}
			_, err := backupDriver.CreateCloudCredential(credCreateRequest)
			Expect(err).NotTo(HaveOccurred(),
				fmt.Sprintf("Failed to create cloud credential [%s] in org [%s]", name, orgID))
			// TODO: validate CreateCloudCredentialResponse also
		}
	})

}

func CreateBackupLocation(provider, name, credName, orgID string) {
	switch provider {
	case providerAws:
		createS3BackupLocation(name, credName, orgID)
	}
}

// createS3BackupLocation creates backup location
func createS3BackupLocation(name string, cloudCred string, orgID string) {
	Step(fmt.Sprintf("Create S3 backup location [%s] in org [%s]", name, orgID), func() {
		CreateS3BackupLocation(name, cloudCred, orgID)
	})
}

// createS3BackupLocation creates backup location
func createAzureBackupLocation(name string, cloudCred string, orgID string) {
	Step(fmt.Sprintf("Create Azure backup location [%s] in org [%s]", name, orgID), func() {
		// TODO(stgleb): Implement this
	})
}

// createS3BackupLocation creates backup location
func createGkeBackupLocation(name string, cloudCred string, orgID string) {
	Step(fmt.Sprintf("Create GKE backup location [%s] in org [%s]", name, orgID), func() {
		// TODO(stgleb): Implement this
	})
}

// CreateS3BackupLocation creates backuplocation for S3
func CreateS3BackupLocation(name string, cloudCred string, orgID string) {
	backupDriver := Inst().Backup
	path := os.Getenv("BUCKET_NAME")
	Expect(path).NotTo(Equal(""),
		"BUCKET_NAME Environment variable should not be empty")

	endpoint := os.Getenv("S3_ENDPOINT")
	Expect(endpoint).NotTo(Equal(""),
		"S3_ENDPOINT Environment variable should not be empty")

	region := os.Getenv("S3_REGION")
	Expect(endpoint).NotTo(Equal(""),
		"S3_REGION Environment variable should not be empty")

	disableSSL := os.Getenv("S3_DISABLE_SSL")
	Expect(endpoint).NotTo(Equal(""),
		"S3_DISABLE_SSL Environment variable should not be empty")
	disableSSLBool, err := strconv.ParseBool(disableSSL)
	Expect(err).NotTo(HaveOccurred(),
		fmt.Sprintf("S3_DISABLE_SSL=%s is not a valid boolean value", disableSSL))

	encryptionKey := "torpedo"
	bLocationCreateReq := &api.BackupLocationCreateRequest{
		CreateMetadata: &api.CreateMetadata{
			Name:  name,
			OrgId: orgID,
		},
		BackupLocation: &api.BackupLocationInfo{
			Path:            path,
			EncryptionKey:   encryptionKey,
			CloudCredential: cloudCred,
			Type:            api.BackupLocationInfo_S3,
			Config: &api.BackupLocationInfo_S3Config{
				S3Config: &api.S3Config{
					Endpoint:   endpoint,
					Region:     region,
					DisableSsl: disableSSLBool,
				},
			},
		},
	}
	_, err = backupDriver.CreateBackupLocation(bLocationCreateReq)
	Expect(err).NotTo(HaveOccurred(),
		fmt.Sprintf("Failed to create backuplocation [%s] in org [%s]", name, orgID))
}

// DeleteBackupLocation deletes backuplocation
func DeleteBackupLocation(name string, orgID string) {
	Step(fmt.Sprintf("Delete backup location [%s] in org [%s]", name, orgID), func() {
		backupDriver := Inst().Backup
		bLocationDeleteReq := &api.BackupLocationDeleteRequest{
			Name:  name,
			OrgId: orgID,
		}
		backupDriver.DeleteBackupLocation(bLocationDeleteReq)
		// Best effort cleanup, dont fail test, if deletion fails
		//Expect(err).NotTo(HaveOccurred(),
		//	fmt.Sprintf("Failed to delete backup location [%s] in org [%s]", name, orgID))
		// TODO: validate createBackupLocationResponse also
	})
}

// CreateSourceAndDestClusters creates source and destination cluster
// 1st cluster in KUBECONFIGS ENV var is source cluster while
// 2nd cluster is destination cluster
func CreateSourceAndDestClusters(provider, cloudCred, orgID string) {
	if provider == "" {
		return
	}

	// TODO: Add support for adding multiple clusters from
	// comma separated list of kubeconfig files
	kubeconfigs := os.Getenv("KUBECONFIGS")
	Expect(kubeconfigs).NotTo(Equal(""),
		"KUBECONFIGS Environment variable should not be empty")

	kubeconfigList := strings.Split(kubeconfigs, ",")
	// Validate user has provided at least 2 kubeconfigs for source and destination cluster
	Expect(len(kubeconfigList)).Should(BeNumerically(">=", 2), "At least minimum two kubeconfigs required")

	err := dumpKubeConfigs(ConfigMapName, kubeconfigList)
	Expect(err).NotTo(HaveOccurred(),
		fmt.Sprintf("Failed to get kubeconfigs [%v] from configmap [%s]", kubeconfigList, ConfigMapName))

	// Register source cluster with backup driver
	Step(fmt.Sprintf("Create cluster [%s] in org [%s]", SourceClusterName, orgID), func() {
		srcClusterConfigPath, err := getSourceClusterConfigPath()
		Expect(err).NotTo(HaveOccurred(),
			fmt.Sprintf("Failed to get kubeconfig path for source cluster. Error: [%v]", err))

		CreateCluster(SourceClusterName, cloudCred, srcClusterConfigPath, orgID)
	})

	// Register destination cluster with backup driver
	Step(fmt.Sprintf("Create cluster [%s] in org [%s]", DestinationClusterName, orgID), func() {
		dstClusterConfigPath, err := getDestinationClusterConfigPath()
		Expect(err).NotTo(HaveOccurred(),
			fmt.Sprintf("Failed to get kubeconfig path for destination cluster. Error: [%v]", err))
		CreateCluster(DestinationClusterName, cloudCred, dstClusterConfigPath, orgID)
	})
}

func getSourceClusterConfigPath() (string, error) {
	kubeconfigs := os.Getenv("KUBECONFIGS")
	if kubeconfigs == "" {
		return "", fmt.Errorf("Empty KUBECONFIGS environment variable")
	}

	kubeconfigList := strings.Split(kubeconfigs, ",")
	Expect(len(kubeconfigList)).Should(BeNumerically(">=", 2),
		"At least minimum two kubeconfigs required")

	return fmt.Sprintf("%s/%s", KubeconfigDirectory, kubeconfigList[0]), nil
}

func getDestinationClusterConfigPath() (string, error) {
	kubeconfigs := os.Getenv("KUBECONFIGS")
	if kubeconfigs == "" {
		return "", fmt.Errorf("Empty KUBECONFIGS environment variable")
	}

	kubeconfigList := strings.Split(kubeconfigs, ",")
	Expect(len(kubeconfigList)).Should(BeNumerically(">=", 2),
		"At least minimum two kubeconfigs required")

	return fmt.Sprintf("%s/%s", KubeconfigDirectory, kubeconfigList[1]), nil
}

func dumpKubeConfigs(configObject string, kubeconfigList []string) error {
	cm, err := core.Instance().GetConfigMap(configObject, "default")
	if err != nil {
		logrus.Errorf("Error reading config map: %v", err)
		return err
	}
	for _, kubeconfig := range kubeconfigList {

		config := cm.Data[kubeconfig]
		if len(config) == 0 {
			configErr := fmt.Sprintf("Error reading kubeconfig: found empty %s in config map %s",
				kubeconfig, configObject)
			return fmt.Errorf(configErr)
		}
		filePath := fmt.Sprintf("%s/%s", KubeconfigDirectory, kubeconfig)
		err := ioutil.WriteFile(filePath, []byte(config), 0644)
		if err != nil {
			return err
		}
	}
	return nil
}

// DeleteCluster deletes/de-registers cluster from px-backup
func DeleteCluster(name string, orgID string) {

	Step(fmt.Sprintf("Delete cluster [%s] in org [%s]", name, orgID), func() {
		backupDriver := Inst().Backup
		clusterDeleteReq := &api.ClusterDeleteRequest{
			OrgId: orgID,
			Name:  name,
		}
		backupDriver.DeleteCluster(clusterDeleteReq)
		// Best effort cleanup, dont fail test, if deletion fails
		//Expect(err).NotTo(HaveOccurred(),
		//	fmt.Sprintf("Failed to delete cluster [%s] in org [%s]", name, orgID))
	})
}

// CreateCluster creates/registers cluster with px-backup
func CreateCluster(name string, cloudCred string, kubeconfigPath string, orgID string) {

	Step(fmt.Sprintf("Create cluster [%s] in org [%s]", name, orgID), func() {
		backupDriver := Inst().Backup
		kubeconfigRaw, err := ioutil.ReadFile(kubeconfigPath)
		Expect(err).NotTo(HaveOccurred(),
			fmt.Sprintf("Failed to read kubeconfig file from location [%s]. Error:[%v]",
				kubeconfigPath, err))

		clusterCreateReq := &api.ClusterCreateRequest{
			CreateMetadata: &api.CreateMetadata{
				Name:  name,
				OrgId: orgID,
			},
			Kubeconfig:      base64.StdEncoding.EncodeToString(kubeconfigRaw),
			CloudCredential: cloudCred,
		}

		_, err = backupDriver.CreateCluster(clusterCreateReq)
		Expect(err).NotTo(HaveOccurred(),
			fmt.Sprintf("Failed to create cluster [%s] in org [%s]. Error : [%v]",
				name, orgID, err))
	})
}

// CreateBackup creates backup
func CreateBackup(backupName string, clusterName string, bLocation string,
	namespaces []string, labelSelectores map[string]string, orgID string) {

	Step(fmt.Sprintf("Create backup [%s] in org [%s] from cluster [%s]",
		backupName, orgID, clusterName), func() {

		backupDriver := Inst().Backup
		bkpCreateRequest := &api.BackupCreateRequest{
			CreateMetadata: &api.CreateMetadata{
				Name:  backupName,
				OrgId: orgID,
			},
			BackupLocation: bLocation,
			Cluster:        clusterName,
			Namespaces:     namespaces,
			LabelSelectors: labelSelectores,
		}
		_, err := backupDriver.CreateBackup(bkpCreateRequest)
		Expect(err).NotTo(HaveOccurred(),
			fmt.Sprintf("Failed to create backup [%s] in org [%s]", backupName, orgID))
		// TODO: validate createClusterResponse also

	})
}

// CreateRestore creates restore
func CreateRestore(restoreName string, backupName string,
	namespaceMapping map[string]string, clusterName string, orgID string) {

	Step(fmt.Sprintf("Create restore [%s] in org [%s] on cluster [%s]",
		restoreName, orgID, clusterName), func() {

		backupDriver := Inst().Backup
		createRestoreReq := &api.RestoreCreateRequest{
			CreateMetadata: &api.CreateMetadata{
				Name:  restoreName,
				OrgId: orgID,
			},
			Backup:           backupName,
			Cluster:          clusterName,
			NamespaceMapping: namespaceMapping,
		}
		_, err := backupDriver.CreateRestore(createRestoreReq)
		Expect(err).NotTo(HaveOccurred(),
			fmt.Sprintf("Failed to create restore [%s] in org [%s] on cluster [%s]",
				restoreName, orgID, clusterName))
		// TODO: validate createClusterResponse also
	})
}

// DeleteBackup deletes backup
func DeleteBackup(backupName string, clusterName string, orgID string) {

	Step(fmt.Sprintf("Delete backup [%s] in org [%s] from cluster [%s]",
		backupName, orgID, clusterName), func() {

		backupDriver := Inst().Backup
		bkpDeleteRequest := &api.BackupDeleteRequest{
			Name:  backupName,
			OrgId: orgID,
		}
		backupDriver.DeleteBackup(bkpDeleteRequest)
		// Best effort cleanup, dont fail test, if deletion fails
		//Expect(err).NotTo(HaveOccurred(),
		//	fmt.Sprintf("Failed to delete backup [%s] in org [%s]", backupName, orgID))
		// TODO: validate createClusterResponse also
	})
}

// DeleteRestore creates restore
func DeleteRestore(restoreName string, orgID string) {

	Step(fmt.Sprintf("Delete restore [%s] in org [%s]",
		restoreName, orgID), func() {

		backupDriver := Inst().Backup
		Expect(backupDriver).NotTo(BeNil(),
			"Backup driver is not initialized")

		deleteRestoreReq := &api.RestoreDeleteRequest{
			OrgId: orgID,
			Name:  restoreName,
		}
		_, err := backupDriver.DeleteRestore(deleteRestoreReq)
		Expect(err).NotTo(HaveOccurred(),
			fmt.Sprintf("Failed to delete restore [%s] in org [%s]",
				restoreName, orgID))
		// TODO: validate createClusterResponse also
	})
}
