/*


Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
	"golang.org/x/oauth2/clientcredentials"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"

	crd "github.com/RedHatInsights/clowder/apis/cloud.redhat.com/v1alpha1"
	"github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/clowderconfig"
	"github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/config"
	"github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers"
	"github.com/RedHatInsights/clowder/controllers/cloud.redhat.com/providers/kafka"
	"github.com/RedHatInsights/rhc-osdk-utils/utils"
	strimzi "github.com/RedHatInsights/strimzi-client-go/apis/kafka.strimzi.io/v1beta2"
	keda "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	// +kubebuilder:scaffold:imports
)

var k8sClient client.Client
var testEnv *envtest.Environment
var logger *zap.Logger

type TestSuite struct {
	suite.Suite
	stopController context.CancelFunc
}

func runAPITestServer() {
	_ = CreateAPIServer().ListenAndServe()
}

func loggerSync(log *zap.Logger) {
	// Ignore the error from sync
	_ = log.Sync()
}

func (suite *TestSuite) SetupSuite() {
	// call flag.Parse() here if TestMain uses flags
	ctrl.SetLogger(ctrlzap.New(ctrlzap.UseDevMode(true)))
	logger, _ = zap.NewProduction()
	defer loggerSync(logger)
	logger.Info("bootstrapping test environment")

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),  // generated by controller-gen
			filepath.Join("..", "..", "config", "crd", "static"), // added to the project manually
		},
	}

	cfg, err := testEnv.Start()

	if err != nil {
		logger.Fatal("Error starting test env", zap.Error(err))
	}

	assert.NotNil(suite.T(), cfg, "env config was returned nil")

	err = crd.AddToScheme(clientgoscheme.Scheme)
	assert.NoError(suite.T(), err, "failed to add scheme")

	err = strimzi.AddToScheme(clientgoscheme.Scheme)
	assert.NoError(suite.T(), err, "failed to add scheme")

	err = keda.AddToScheme(clientgoscheme.Scheme)
	assert.NoError(suite.T(), err, "failed to add scheme")

	// +kubebuilder:scaffold:scheme

	k8sClient, err = client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	assert.NoError(suite.T(), err, "failed to create k8s client")
	assert.NotNil(suite.T(), k8sClient, "k8sClient was returned nil")

	// ctx := context.Background()

	ctx, stopController := context.WithCancel(context.Background())
	suite.stopController = stopController

	nsSpec := &core.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kafka"}}
	err = k8sClient.Create(ctx, nsSpec)
	assert.NoError(suite.T(), err, "error creating namespace")

	go Run(ctx, ":8080", ":8081", false, testEnv.Config, false)
	go runAPITestServer()

	for i := 1; i <= 50; i++ {
		resp, err := http.Get("http://localhost:8080/metrics")

		if err == nil && resp.StatusCode == 200 {
			logger.Info("Manager ready", zap.Int("duration", 100*i))
			defer resp.Body.Close()
			return
		} else if err == nil {
			defer resp.Body.Close()
		}

		if i == 50 {
			if err != nil {
				logger.Fatal("Failed to fetch to metrics for manager after 5s", zap.Error(err))
			}

			logger.Fatal("Failed to get 200 result for metrics", zap.Int("status", resp.StatusCode))
		}

		time.Sleep(100 * time.Millisecond)
	}
}

func (suite *TestSuite) TearDownSuite() {
	logger.Info("Stopping test env...")

	suite.stopController()

	err := testEnv.Stop()

	if err != nil {
		logger.Fatal("Failed to tear down env", zap.Error(err))
	}
}

func applyKafkaStatus(t *testing.T, ch chan int) {
	ctx := context.Background()
	nn := types.NamespacedName{
		Name:      "kafka",
		Namespace: "kafka",
	}
	host := "kafka-bootstrap.kafka.svc"
	listenerType := "plain"
	kport := int32(9092)

	// this loop will run for 60sec max
	for i := 1; i < 1200; i++ {
		if t.Failed() {
			break
		}
		t.Logf("Loop in applyKafkaStatus")
		time.Sleep(50 * time.Millisecond)

		// set a mock status on strimzi Kafka cluster
		cluster := strimzi.Kafka{}
		err := k8sClient.Get(ctx, nn, &cluster)

		if err != nil {
			t.Logf(err.Error())
			continue
		}

		cluster.Status = &strimzi.KafkaStatus{
			Conditions: []strimzi.KafkaStatusConditionsElem{{
				Status: utils.StringPtr("True"),
				Type:   utils.StringPtr("Ready"),
			}},
			Listeners: []strimzi.KafkaStatusListenersElem{{
				Type: &listenerType,
				Addresses: []strimzi.KafkaStatusListenersElemAddressesElem{{
					Host: &host,
					Port: &kport,
				}},
			}},
		}
		t.Logf("Applying kafka status")
		err = k8sClient.Status().Update(ctx, &cluster)

		if err != nil {
			t.Logf(err.Error())
			continue
		}

		// set a mock status on strimzi KafkaConnect cluster
		connectCluster := strimzi.KafkaConnect{}
		nn := types.NamespacedName{
			Name:      "kafka",
			Namespace: "kafka",
		}
		err = k8sClient.Get(ctx, nn, &connectCluster)

		if err != nil {
			t.Logf(err.Error())
			continue
		}

		connectCluster.Status = &strimzi.KafkaConnectStatus{
			Conditions: []strimzi.KafkaConnectStatusConditionsElem{{
				Status: utils.StringPtr("True"),
				Type:   utils.StringPtr("Ready"),
			}},
		}
		t.Logf("Applying kafka connect status")
		err = k8sClient.Status().Update(ctx, &connectCluster)

		if err != nil {
			t.Logf(err.Error())
			continue
		}

		break
	}

	ch <- 0
}

func createCloudwatchSecret(cwData *map[string]string) error {
	cloudwatch := core.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cloudwatch",
			Namespace: "default",
		},
		StringData: *cwData,
	}

	return k8sClient.Create(context.Background(), &cloudwatch)
}

func createClowdEnvironment(objMeta metav1.ObjectMeta) crd.ClowdEnvironment {
	env := crd.ClowdEnvironment{
		ObjectMeta: objMeta,
		Spec: crd.ClowdEnvironmentSpec{
			Providers: crd.ProvidersConfig{
				Kafka: crd.KafkaConfig{
					Mode: "operator",
					Cluster: crd.KafkaClusterConfig{
						Name:      "kafka",
						Namespace: "kafka",
						Replicas:  5,
					},
				},
				Database: crd.DatabaseConfig{
					Mode: "local",
				},
				Logging: crd.LoggingConfig{
					Mode: "app-interface",
				},
				ObjectStore: crd.ObjectStoreConfig{
					Mode: "app-interface",
				},
				InMemoryDB: crd.InMemoryDBConfig{
					Mode: "redis",
				},
				Web: crd.WebConfig{
					Port: int32(8000),
					Mode: "none",
				},
				Metrics: crd.MetricsConfig{
					Port: int32(9000),
					Path: "/metrics",
					Mode: "none",
				},
				FeatureFlags: crd.FeatureFlagsConfig{
					Mode: "none",
				},
				Testing: crd.TestingConfig{
					ConfigAccess:   "environment",
					K8SAccessLevel: "edit",
					Iqe: crd.IqeConfig{
						ImageBase: "quay.io/cloudservices/iqe-tests",
					},
				},
				AutoScaler: crd.AutoScalerConfig{
					Mode: "enabled",
				},
			},
			TargetNamespace: objMeta.Namespace,
		},
	}
	return env
}

func createClowdApp(env crd.ClowdEnvironment, objMeta metav1.ObjectMeta) (crd.ClowdApp, error) {

	ctx := context.Background()

	replicas := int32(32)
	maxReplicas := int32(64)
	partitions := int32(5)
	dbVersion := int32(12)
	topicName := "inventory"

	kafkaTopics := []crd.KafkaTopicSpec{
		{
			TopicName:  topicName,
			Partitions: partitions,
			Replicas:   replicas,
		},
		{
			TopicName: fmt.Sprintf("%s-default-values", topicName),
		},
	}

	app := crd.ClowdApp{
		ObjectMeta: objMeta,
		Spec: crd.ClowdAppSpec{
			Deployments: []crd.Deployment{{
				PodSpec: crd.PodSpec{
					Image: "test:test",
				},
				Name: "testpod",
				AutoScaler: &crd.AutoScaler{
					MaxReplicaCount: &maxReplicas,
					Triggers: []keda.ScaleTriggers{
						{
							Type: "cpu",
							Metadata: map[string]string{
								"type":  "Utilization",
								"value": "50",
							},
						},
					}},
			}},
			EnvName:     env.Name,
			KafkaTopics: kafkaTopics,
			Database: crd.DatabaseSpec{
				Version: &dbVersion,
				Name:    "test",
			},
		},
	}

	err := k8sClient.Create(ctx, &env)

	if err != nil {
		return app, err
	}

	err = k8sClient.Create(ctx, &app)

	if err != nil {
		return app, err
	}

	return app, err
}

func createCRs(name types.NamespacedName) (*crd.ClowdEnvironment, *crd.ClowdApp, error) {

	objMeta := metav1.ObjectMeta{
		Name:      name.Name,
		Namespace: name.Namespace,
	}

	env := createClowdEnvironment(objMeta)

	app, err := createClowdApp(env, objMeta)

	return &env, &app, err
}

func createManagedKafkaClowderStack(name types.NamespacedName, secretNme string) (*crd.ClowdEnvironment, *crd.ClowdApp, error) {
	objMeta := metav1.ObjectMeta{
		Name:      "ephemeral-managed-kafka-name",
		Namespace: name.Namespace,
	}

	env := createClowdEnvironment(objMeta)

	env.Spec.Providers.Kafka = crd.KafkaConfig{
		Mode: "managed-ephem",
		EphemManagedSecretRef: crd.NamespacedName{
			Name:      secretNme,
			Namespace: name.Namespace,
		},
	}

	app, err := createClowdApp(env, objMeta)

	return &env, &app, err
}

func fetchConfig(name types.NamespacedName) (*config.AppConfig, error) {

	secretConfig := core.Secret{}
	jsonContent := config.AppConfig{}

	err := fetchWithDefaults(name, &secretConfig)

	if err != nil {
		return &jsonContent, err
	}

	err = json.Unmarshal(secretConfig.Data["cdappconfig.json"], &jsonContent)

	return &jsonContent, err
}

func (suite *TestSuite) TestCreateClowdApp() {
	logger.Info("Creating ClowdApp")

	clowdAppNN := types.NamespacedName{
		Name:      "test",
		Namespace: "default",
	}

	cwData := map[string]string{
		"aws_access_key_id":     "key_id",
		"aws_secret_access_key": "secret",
		"log_group_name":        "default",
		"aws_region":            "us-east-1",
	}

	err := createCloudwatchSecret(&cwData)
	assert.NoError(suite.T(), err)

	ch := make(chan int)

	go applyKafkaStatus(suite.T(), ch)

	env, app, err := createCRs(clowdAppNN)

	assert.NoError(suite.T(), err)

	<-ch // wait for kafka status to be applied

	labels := map[string]string{
		"app": app.Name,
		"pod": fmt.Sprintf("%s-%s", app.Name, app.Spec.Deployments[0].Name),
	}

	// See if Deployment is created

	d := apps.Deployment{}

	appnn := types.NamespacedName{
		Name:      fmt.Sprintf("%s-%s", app.Name, app.Spec.Deployments[0].Name),
		Namespace: clowdAppNN.Namespace,
	}
	err = fetchWithDefaults(appnn, &d)

	assert.NoError(suite.T(), err)

	assert.Equal(suite.T(), d.Labels, labels, "deployment label mismatch")

	antiAffinity := d.Spec.Template.Spec.Affinity.PodAntiAffinity
	terms := antiAffinity.PreferredDuringSchedulingIgnoredDuringExecution

	assert.Equal(suite.T(), 2, len(terms), "incorrect number of anti-affinity terms")

	c := d.Spec.Template.Spec.Containers[0]

	assert.Equal(suite.T(), app.Spec.Deployments[0].PodSpec.Image, c.Image, "bad image spec")

	// See if Secret is mounted

	found := false
	for _, mount := range c.VolumeMounts {
		if mount.Name == "config-secret" {
			found = true
			break
		}
	}

	assert.True(suite.T(), found, "deployment %s does not have the config volume mounted", d.Name)

	s := core.Service{}

	err = fetchWithDefaults(appnn, &s)
	assert.NoError(suite.T(), err)
	assert.Equal(suite.T(), s.Labels, labels, "service label mismatch")
	assert.Equal(suite.T(), len(s.Spec.Ports), 1, "bad port count")
	assert.Equal(suite.T(), env.Spec.Providers.Metrics.Port, s.Spec.Ports[0].Port, "bad port created")

	jsonContent, err := fetchConfig(clowdAppNN)

	assert.NoError(suite.T(), err)

	metadataValidation(suite.T(), app, jsonContent)

	kafkaValidation(suite.T(), env, app, jsonContent)

	clowdWatchValidation(suite.T(), jsonContent, cwData)

	scaler := keda.ScaledObject{}

	err = fetchWithDefaults(appnn, &scaler)

	assert.NoError(suite.T(), err)

	scaledObjectValidation(suite.T(), &scaler)

	resp, err := http.Get("http://127.0.0.1:2019/config/")
	assert.NoError(suite.T(), err, "failed test because get failed")
	defer resp.Body.Close()

	config := clowderconfig.ClowderConfig{}
	sData, _ := io.ReadAll(resp.Body)
	err = json.Unmarshal(sData, &config)

	assert.NoError(suite.T(), err, "failed test because API not available")

	resp, err = http.Get("http://127.0.0.1:2019/clowdapps/present/")
	assert.NoError(suite.T(), err, "failed test because get failed")
	defer resp.Body.Close()
	capps := []string{}
	sData, _ = io.ReadAll(resp.Body)
	err = json.Unmarshal(sData, &capps)

	assert.NoError(suite.T(), err, "failed to unmarshal")
	assert.Contains(suite.T(), capps, "test.test", "app not present in API call")
}

func metadataValidation(t *testing.T, app *crd.ClowdApp, jsonContent *config.AppConfig) {
	assert.Equal(t, *jsonContent.Metadata.Name, app.Name)
	assert.Equal(t, *jsonContent.Metadata.EnvName, app.Spec.EnvName)

	for _, deployment := range app.Spec.Deployments {
		expected := config.DeploymentMetadata{
			Name:  deployment.Name,
			Image: deployment.PodSpec.Image,
		}
		assert.Contains(t, jsonContent.Metadata.Deployments, expected)
	}
	assert.Len(t, jsonContent.Metadata.Deployments, len(app.Spec.Deployments))
}

func kafkaValidation(t *testing.T, env *crd.ClowdEnvironment, app *crd.ClowdApp, jsonContent *config.AppConfig) {
	// Kafka validation

	topicWithPartitionsReplicasName := "inventory"
	topicWithPartitionsReplicasNamespacedName := types.NamespacedName{
		Namespace: env.Spec.Providers.Kafka.Cluster.Namespace,
		Name:      topicWithPartitionsReplicasName,
	}

	topicNoPartitionsReplicasName := "inventory-default-values"
	topicNoPartitionsReplicasNamespacedName := types.NamespacedName{
		Namespace: env.Spec.Providers.Kafka.Cluster.Namespace,
		Name:      topicNoPartitionsReplicasName,
	}

	for i, kafkaTopic := range app.Spec.KafkaTopics {
		assert.Equal(t, kafkaTopic.TopicName, jsonContent.Kafka.Topics[i].RequestedName, "wrong topic name set on app's config")
		assert.Equal(t, kafkaTopic.TopicName, jsonContent.Kafka.Topics[i].Name, "wrong generated topic name set on app's config")
	}

	assert.NotEqual(t, len(jsonContent.Kafka.Brokers[0].Hostname), 0, "kafka broker hostname is not set")

	for _, topic := range []types.NamespacedName{topicWithPartitionsReplicasNamespacedName, topicNoPartitionsReplicasNamespacedName} {
		fetchedTopic := strimzi.KafkaTopic{}

		// fetch topic, make sure it was provisioned
		err := fetchWithDefaults(topic, &fetchedTopic)
		assert.NoError(t, err, "error fetching topic")
		assert.NotNil(t, fetchedTopic.Spec, "kafkaTopic '%s' not provisioned in namespace", topic.Name)

		// check that configured partitions/replicas matches
		expectedReplicas := int32(0)
		expectedPartitions := int32(0)
		if topic.Name == topicWithPartitionsReplicasName {
			expectedReplicas = int32(5)
			expectedPartitions = int32(5)
		}
		if topic.Name == topicNoPartitionsReplicasName {
			expectedReplicas = int32(3)
			expectedPartitions = int32(3)
		}

		assert.Equal(t, *fetchedTopic.Spec.Replicas, expectedReplicas, "bad topic replica count for '%s': %d; expected %d", topic.Name, fetchedTopic.Spec.Replicas, expectedReplicas)
		assert.Equal(t, *fetchedTopic.Spec.Partitions, expectedPartitions, "bad topic replica count for '%s': %d; expected %d", topic.Name, fetchedTopic.Spec.Partitions, expectedPartitions)
	}
}

func clowdWatchValidation(t *testing.T, jsonContent *config.AppConfig, cwData map[string]string) {
	// Cloudwatch validation
	cwConfigVals := map[string]string{
		"aws_access_key_id":     jsonContent.Logging.Cloudwatch.AccessKeyId,
		"aws_secret_access_key": jsonContent.Logging.Cloudwatch.SecretAccessKey,
		"log_group_name":        jsonContent.Logging.Cloudwatch.LogGroup,
		"aws_region":            jsonContent.Logging.Cloudwatch.Region,
	}

	for key, val := range cwData {
		assert.Equal(t, val, cwConfigVals[key], "wrong cloudwatch config value")
	}
}

func scaledObjectValidation(t *testing.T, scaler *keda.ScaledObject) {
	// Scaled object validation
	expectTarget := keda.ScaleTarget{
		Kind: "Deployment",
		Name: "test-testpod",
	}
	expectedTrigger := keda.ScaleTriggers{
		Type: "cpu",
		Metadata: map[string]string{
			"type":  "Utilization",
			"value": "50",
		},
	}
	for _, trigger := range scaler.Spec.Triggers {
		assert.Equal(t, expectedTrigger.Type, trigger.Type)
		assert.Equal(t, expectedTrigger.Metadata, trigger.Metadata)
	}

	assert.Equal(t, expectTarget.Kind, scaler.Spec.ScaleTargetRef.Kind)
	assert.Equal(t, expectTarget.Name, scaler.Spec.ScaleTargetRef.Name)
}

func fetchWithDefaults(name types.NamespacedName, resource client.Object) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	return fetch(ctx, name, resource, 20*6, 500*time.Millisecond)
}

func fetch(ctx context.Context, name types.NamespacedName, resource client.Object, retryCount int, sleepTime time.Duration) error {
	var err error

	for i := 1; i <= retryCount; i++ {
		err = k8sClient.Get(ctx, name, resource)

		if err == nil {
			return nil
		} else if !k8serr.IsNotFound(err) {
			return err
		}

		time.Sleep(sleepTime)
	}

	return err
}

func createEphemeralManagedSecret(name string, namespace string, cwData map[string]string) error {
	secret := core.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		StringData: cwData,
	}

	return k8sClient.Create(context.Background(), &secret)
}

var ephemManagedKafkaHTTPLog = []map[string]string{}

func (suite *TestSuite) TestManagedKafkaConnectBuilderCreate() {
	logger.Info("Starting ephemeral managed kafka e2e test")

	nn := types.NamespacedName{
		Name:      "managed-ephemeral-kafka",
		Namespace: "default",
	}

	secretData := make(map[string]string)
	secretData["client.id"] = "username"
	secretData["client.secret"] = "password"
	secretData["hostname"] = "hostname:443"
	secretData["admin.url"] = "https://admin.url"
	secretData["token.url"] = "https://token.url"
	secName := "managed-ephem-secret"
	err := createEphemeralManagedSecret(secName, nn.Namespace, secretData)
	assert.Nil(suite.T(), err)

	cwData := map[string]string{
		"aws_access_key_id":     "key_id",
		"aws_secret_access_key": "secret",
		"log_group_name":        "default",
		"aws_region":            "us-east-1",
	}

	_ = createCloudwatchSecret(&cwData)

	mockClient := MockEphemManagedKafkaHTTPClient{topicList: make(map[string]bool)}

	kafka.ClientCreator = func(provider *providers.Provider, clientCred clientcredentials.Config) kafka.HTTPClient {
		return &mockClient
	}

	env, app, err := createManagedKafkaClowderStack(nn, secName)

	assert.Nil(suite.T(), err)

	assert.NotNil(suite.T(), app)
	assert.NotNil(suite.T(), env)

	mockClient.createStaticTopic("ephemeral-dont-delete")
	mockClient.createStaticTopic("ephemeral.managed.kafka.name.inventory")

	ephemManagedSecret := env.Spec.Providers.Kafka.EphemManagedSecretRef
	assert.Equal(suite.T(), secName, ephemManagedSecret.Name)
	assert.Equal(suite.T(), nn.Namespace, ephemManagedSecret.Namespace)
	assert.Eventually(suite.T(), func() bool {
		return assert.Contains(suite.T(), mockClient.topicList, "ephemeral-managed-kafka-name-inventory")
	}, time.Second*15, time.Second*1)
	assert.Eventually(suite.T(), func() bool {
		return assert.Contains(suite.T(), mockClient.topicList, "ephemeral-managed-kafka-name-inventory-default-values")
	}, time.Second*15, time.Second*1)

	ctx := context.Background()
	err = k8sClient.Delete(ctx, env)
	assert.NoError(suite.T(), err, "couldn't delete resource")

	assert.Eventually(suite.T(), func() bool {
		return assert.NotContains(suite.T(), mockClient.topicList, "ephemeral-managed-kafka-name-inventory")
	}, time.Second*15, time.Second*1)
	assert.Eventually(suite.T(), func() bool {
		return assert.NotContains(suite.T(), mockClient.topicList, "ephemeral-managed-kafka-name-inventory-default-values")
	}, time.Second*15, time.Second*1)
	assert.Eventually(suite.T(), func() bool {
		return assert.NotContains(suite.T(), mockClient.topicList, "ephemeral.managed.kafka.name.inventory")
	}, time.Second*15, time.Second*1)
	assert.Eventually(suite.T(), func() bool {
		return assert.Contains(suite.T(), mockClient.topicList, "ephemeral-dont-delete")
	}, time.Second*15, time.Second*1)
}

type MockEphemManagedKafkaHTTPClient struct {
	topicList map[string]bool
}

func (m *MockEphemManagedKafkaHTTPClient) createStaticTopic(topicName string) {
	m.topicList[topicName] = true
}

func (m *MockEphemManagedKafkaHTTPClient) makeResp(body string, code int) http.Response {
	readBody := io.NopCloser(strings.NewReader(body))
	resp := http.Response{
		Status:           fmt.Sprint(code),
		StatusCode:       code,
		Proto:            "HTTP/1.0",
		ProtoMajor:       0,
		ProtoMinor:       0,
		Header:           make(http.Header, 0),
		Body:             readBody,
		ContentLength:    int64(len(body)),
		TransferEncoding: []string{},
		Close:            false,
		Uncompressed:     false,
		Trailer:          map[string][]string{},
		Request:          &http.Request{},
		TLS:              &tls.ConnectionState{},
	}
	// buff := bytes.NewBuffer(nil)
	// resp.Write(buff)
	return resp
}

func (m *MockEphemManagedKafkaHTTPClient) logResponse(resp *http.Response) {
	entry := map[string]string{}
	entry["status"] = resp.Status
	entry["code"] = strconv.Itoa(resp.StatusCode)
	// This is on purpose because whenever I try to read the body I get 0 bytes
	// and I messed with it for too long and I don't care anymore because this is a
	// test not the ISS's attitude control system
	entry["body"] = resp.Proto

	ephemManagedKafkaHTTPLog = append(ephemManagedKafkaHTTPLog, entry)

}

func (m *MockEphemManagedKafkaHTTPClient) Do(req *http.Request) (*http.Response, error) {
	url := req.URL.String()
	var resp *http.Response
	if req.Method == "PATCH" {
		r := m.makeResp(`{"msg":"topic patched"}`, 200)
		resp = &r
	} else if req.Method == "DELETE" {
		items := strings.Split(url, "/")
		delete(m.topicList, items[len(items)-1])
		r := m.makeResp(`{"msg":"topic found"}`, 200)
		resp = &r
	}
	m.logResponse(resp)
	return resp, nil
}

func (m *MockEphemManagedKafkaHTTPClient) Get(url string) (*http.Response, error) {
	var resp http.Response
	items := strings.Split(url, "/")
	if len(items) == 6 {
		tlist := kafka.TopicsList{}
		for k := range m.topicList {
			tlist.Items = append(tlist.Items, kafka.Topic{Name: k})
		}
		body, err := json.Marshal(tlist)
		if err != nil {
			return nil, fmt.Errorf("can not list topics: %s", err)
		}
		resp = m.makeResp(string(body), 200)
	} else if len(items) == 7 {
		for k := range m.topicList {
			if items[len(items)-1] == k {
				resp = m.makeResp(`{"msg":"topic found"}`, 200)
				break
			}
		}
		resp = m.makeResp(`{"msg":"topic not found"}`, 404)
	}

	m.logResponse(&resp)
	return &resp, nil
}

func (m *MockEphemManagedKafkaHTTPClient) Post(_, _ string, body io.Reader) (*http.Response, error) {
	bodyData, _ := io.ReadAll(body)
	kafkaObj := &kafka.JSONPayload{}
	err := json.Unmarshal(bodyData, kafkaObj)
	if err != nil {
		return nil, err
	}
	m.topicList[kafkaObj.Name] = true
	resp := m.makeResp(`{"msg":"topic created"}`, 200)
	m.logResponse(&resp)
	return &resp, nil
}

func TestSuiteRun(t *testing.T) {
	suite.Run(t, new(TestSuite))
}
