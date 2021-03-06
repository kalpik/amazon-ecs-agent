// +build !integration
// Copyright 2014-2017 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package engine

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/amazon-ecs-agent/agent/api"
	"github.com/aws/amazon-ecs-agent/agent/config"
	"github.com/aws/amazon-ecs-agent/agent/credentials"
	"github.com/aws/amazon-ecs-agent/agent/credentials/mocks"
	"github.com/aws/amazon-ecs-agent/agent/ecscni/mocks"
	"github.com/aws/amazon-ecs-agent/agent/engine/dockerstate"
	"github.com/aws/amazon-ecs-agent/agent/engine/image"
	"github.com/aws/amazon-ecs-agent/agent/engine/testdata"
	"github.com/aws/amazon-ecs-agent/agent/eventstream"
	"github.com/aws/amazon-ecs-agent/agent/statemanager/mocks"
	"github.com/aws/amazon-ecs-agent/agent/utils/ttime/mocks"
	"github.com/aws/aws-sdk-go/aws"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"golang.org/x/net/context"
)

const (
	credentialsID       = "credsid"
	ipv4                = "10.0.0.1"
	mac                 = "1.2.3.4"
	ipv6                = "f0:234:23"
	containerID         = "containerID"
	dockerContainerName = "docker-container-name"
	containerPid        = 123
)

var defaultConfig = config.DefaultConfig()

func mocks(t *testing.T, cfg *config.Config) (*gomock.Controller, *MockDockerClient, *mock_ttime.MockTime, TaskEngine, *mock_credentials.MockManager, *MockImageManager) {
	ctrl := gomock.NewController(t)
	client := NewMockDockerClient(ctrl)
	mockTime := mock_ttime.NewMockTime(ctrl)
	credentialsManager := mock_credentials.NewMockManager(ctrl)

	containerChangeEventStream := eventstream.NewEventStream("TESTTASKENGINE", context.Background())
	containerChangeEventStream.StartListening()
	imageManager := NewMockImageManager(ctrl)
	taskEngine := NewTaskEngine(cfg, client, credentialsManager, containerChangeEventStream, imageManager, dockerstate.NewTaskEngineState())
	taskEngine.(*DockerTaskEngine)._time = mockTime
	return ctrl, client, mockTime, taskEngine, credentialsManager, imageManager
}

func createDockerEvent(status api.ContainerStatus) DockerContainerChangeEvent {
	meta := DockerContainerMetadata{
		DockerID: containerID,
	}
	return DockerContainerChangeEvent{Status: status, DockerContainerMetadata: meta}
}

func TestBatchContainerHappyPath(t *testing.T) {
	ctrl, client, mockTime, taskEngine, credentialsManager, imageManager := mocks(t, &defaultConfig)
	defer ctrl.Finish()

	roleCredentials := credentials.TaskIAMRoleCredentials{
		IAMRoleCredentials: credentials.IAMRoleCredentials{CredentialsID: credentialsID},
	}
	credentialsManager.EXPECT().GetTaskCredentials(credentialsID).Return(roleCredentials, true).AnyTimes()
	credentialsManager.EXPECT().RemoveCredentials(credentialsID)

	sleepTask := testdata.LoadTask("sleep5")
	sleepTask.SetCredentialsID(credentialsID)

	eventStream := make(chan DockerContainerChangeEvent)
	// createStartEventsReported is used to force the test to wait until the container created and started
	// events are processed
	createStartEventsReported := sync.WaitGroup{}

	client.EXPECT().Version()
	client.EXPECT().ContainerEvents(gomock.Any()).Return(eventStream, nil)
	var createdContainerName string
	for _, container := range sleepTask.Containers {
		imageManager.EXPECT().AddAllImageStates(gomock.Any()).AnyTimes()
		client.EXPECT().PullImage(container.Image, nil).Return(DockerContainerMetadata{})
		imageManager.EXPECT().RecordContainerReference(container).Return(nil)
		imageManager.EXPECT().GetImageStateFromImageName(gomock.Any()).Return(nil)
		dockerConfig, err := sleepTask.DockerConfig(container)
		if err != nil {
			t.Fatal(err)
		}
		// Container config should get updated with this during PostUnmarshalTask
		credentialsEndpointEnvValue := roleCredentials.IAMRoleCredentials.GenerateCredentialsEndpointRelativeURI()
		dockerConfig.Env = append(dockerConfig.Env, "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI="+credentialsEndpointEnvValue)
		// Container config should get updated with this during CreateContainer
		dockerConfig.Labels["com.amazonaws.ecs.task-arn"] = sleepTask.Arn
		dockerConfig.Labels["com.amazonaws.ecs.container-name"] = container.Name
		dockerConfig.Labels["com.amazonaws.ecs.task-definition-family"] = sleepTask.Family
		dockerConfig.Labels["com.amazonaws.ecs.task-definition-version"] = sleepTask.Version
		dockerConfig.Labels["com.amazonaws.ecs.cluster"] = ""
		client.EXPECT().CreateContainer(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Do(
			func(config *docker.Config, y interface{}, containerName string, z time.Duration) {

				if !reflect.DeepEqual(dockerConfig, config) {
					t.Errorf("Mismatch in container config; expected: %v, got: %v", dockerConfig, config)
				}
				// sleep5 task contains only one container. Just assign
				// the containerName to createdContainerName
				createdContainerName = containerName
				createStartEventsReported.Add(1)
				go func() {
					eventStream <- createDockerEvent(api.ContainerCreated)
					createStartEventsReported.Done()
				}()
			}).Return(DockerContainerMetadata{DockerID: containerID})

		client.EXPECT().StartContainer(containerID, startContainerTimeout).Do(
			func(id string, timeout time.Duration) {
				createStartEventsReported.Add(1)
				go func() {
					eventStream <- createDockerEvent(api.ContainerRunning)
					createStartEventsReported.Done()
				}()
			}).Return(DockerContainerMetadata{DockerID: containerID})
	}

	// steadyStateCheckWait is used to force the test to wait until the steady-state check
	// has been invoked at least once
	steadyStateCheckWait := sync.WaitGroup{}
	steadyStateVerify := make(chan time.Time, 1)
	cleanup := make(chan time.Time, 1)
	mockTime.EXPECT().Now().Do(func() time.Time { return time.Now() }).AnyTimes()
	gomock.InOrder(
		mockTime.EXPECT().After(steadyStateTaskVerifyInterval).Do(func(d time.Duration) {
			steadyStateCheckWait.Done()
		}).Return(steadyStateVerify),
		mockTime.EXPECT().After(steadyStateTaskVerifyInterval).Return(steadyStateVerify).AnyTimes(),
	)

	ctx, cancel := context.WithCancel(context.TODO())
	err := taskEngine.Init(ctx)
	assert.NoError(t, err)
	defer cancel()

	stateChangeEvents := taskEngine.StateChangeEvents()

	steadyStateCheckWait.Add(1)
	taskEngine.AddTask(sleepTask)

	event := <-stateChangeEvents
	assert.Equal(t, event.(api.ContainerStateChange).Status, api.ContainerRunning, "Expected container to be RUNNING")

	event = <-stateChangeEvents
	assert.Equal(t, event.(api.TaskStateChange).Status, api.TaskRunning, "Expected task to be RUNNING")

	select {
	case <-stateChangeEvents:
		t.Fatal("Should be out of events")
	default:
	}

	// Wait for all events to be consumed prior to moving it towards stopped; we
	// don't want to race the below with these or we'll end up with the "going
	// backwards in state" stop and we haven't 'expect'd for that

	// Wait for container create and start events to be processed
	createStartEventsReported.Wait()
	// Wait for steady state check to be invoked
	steadyStateCheckWait.Wait()
	mockTime.EXPECT().After(gomock.Any()).Return(cleanup).AnyTimes()
	client.EXPECT().DescribeContainer(gomock.Any()).AnyTimes()

	exitCode := 0
	// And then docker reports that sleep died, as sleep is wont to do
	eventStream <- DockerContainerChangeEvent{
		Status: api.ContainerStopped,
		DockerContainerMetadata: DockerContainerMetadata{
			DockerID: containerID,
			ExitCode: &exitCode,
		},
	}

	event = <-stateChangeEvents
	assert.Equal(t, event.(api.ContainerStateChange).Status, api.ContainerStopped, "Expected container to be STOPPED")

	// hold on to container event to verify exit code
	contEvent := event.(api.ContainerStateChange)
	assert.Equal(t, *contEvent.ExitCode, 0, "Exit code should be present")

	event = <-stateChangeEvents
	assert.Equal(t, event.(api.TaskStateChange).Status, api.TaskStopped, "Expected task to be STOPPED")
	// This ensures that managedTask.waitForStopReported makes progress
	sleepTask.SetSentStatus(api.TaskStopped)

	// Extra events should not block forever; duplicate acs and docker events are possible
	go func() { eventStream <- createDockerEvent(api.ContainerStopped) }()
	go func() { eventStream <- createDockerEvent(api.ContainerStopped) }()

	sleepTaskStop := testdata.LoadTask("sleep5")
	sleepTaskStop.SetCredentialsID(credentialsID)
	sleepTaskStop.SetDesiredStatus(api.TaskStopped)
	taskEngine.AddTask(sleepTaskStop)
	// As above, duplicate events should not be a problem
	taskEngine.AddTask(sleepTaskStop)
	taskEngine.AddTask(sleepTaskStop)

	// Expect a bunch of steady state 'poll' describes when we trigger cleanup
	client.EXPECT().RemoveContainer(gomock.Any(), gomock.Any()).Do(
		func(removedContainerName string, timeout time.Duration) {
			assert.Equal(t, createdContainerName, removedContainerName, "Container name mismatch")
		}).Return(nil)

	imageManager.EXPECT().RemoveContainerReferenceFromImageState(gomock.Any())
	// trigger cleanup
	cleanup <- time.Now()
	go func() { eventStream <- createDockerEvent(api.ContainerStopped) }()

	// Wait for the task to actually be dead; if we just fallthrough immediately,
	// the remove might not have happened (expectation failure)
	for {
		tasks, _ := taskEngine.(*DockerTaskEngine).ListTasks()
		if len(tasks) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestTaskWithSteadyStateResourcesProvisioned tests container and task transitions
// when the steady state for the pause container is set to RESOURCES_PROVISIONED and
// the steady state for the normal container is set to RUNNING
func TestTaskWithSteadyStateResourcesProvisioned(t *testing.T) {
	ctrl, client, mockTime, taskEngine, _, imageManager := mocks(t, &defaultConfig)
	defer ctrl.Finish()

	mockCNIClient := mock_ecscni.NewMockCNIClient(ctrl)
	taskEngine.(*DockerTaskEngine).cniClient = mockCNIClient

	// sleep5 contains a single 'sleep' container, with DesiredStatus == RUNNING
	sleepTask := testdata.LoadTask("sleep5")
	sleepTask.Containers[0].TransitionDependencySet.ContainerDependencies = []api.ContainerDependency{
		{
			ContainerName:   "pause",
			SatisfiedStatus: api.ContainerRunning,
			DependentStatus: api.ContainerPulled,
		}}
	sleepContainer := sleepTask.Containers[0]

	// Add a second container with DesiredStatus == RESOURCES_PROVISIONED and
	// steadyState == RESOURCES_PROVISIONED
	pauseContainer := api.NewContainerWithSteadyState(api.ContainerResourcesProvisioned)
	pauseContainer.Name = "pause"
	pauseContainer.Image = "pause"
	pauseContainer.CPU = 10
	pauseContainer.Memory = 10
	pauseContainer.Essential = true
	pauseContainer.Type = api.ContainerCNIPause
	pauseContainer.DesiredStatusUnsafe = api.ContainerRunning

	sleepTask.Containers = append(sleepTask.Containers, pauseContainer)

	eventStream := make(chan DockerContainerChangeEvent)
	// createStartEventsReported is used to force the test to wait until the container created and started
	// events are processed
	createStartEventsReported := sync.WaitGroup{}

	client.EXPECT().Version()
	client.EXPECT().ContainerEvents(gomock.Any()).Return(eventStream, nil)

	// We cannot rely on the order of pulls between images as they can still be downloaded in
	// parallel. The dependency graph enforcement comes into effect for CREATED transitions.
	// Hence, do not enforce the order of invocation of these calls
	imageManager.EXPECT().AddAllImageStates(gomock.Any()).AnyTimes()
	client.EXPECT().PullImage(sleepContainer.Image, nil).Return(DockerContainerMetadata{})
	imageManager.EXPECT().RecordContainerReference(sleepContainer).Return(nil)
	imageManager.EXPECT().GetImageStateFromImageName(sleepContainer.Image).Return(nil)

	gomock.InOrder(
		// Ensure that the pause container is created first
		client.EXPECT().CreateContainer(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Do(
			func(config *docker.Config, hostConfig *docker.HostConfig, containerName string, z time.Duration) {
				sleepTask.SetTaskENI(&api.ENI{
					ID: "TestTaskWithSteadyStateResourcesProvisioned",
					IPV4Addresses: []*api.ENIIPV4Address{
						{
							Primary: true,
							Address: ipv4,
						},
					},
					MacAddress: mac,
					IPV6Addresses: []*api.ENIIPV6Address{
						{
							Address: ipv6,
						},
					},
				})
				assert.Equal(t, "none", hostConfig.NetworkMode)
				assert.True(t, strings.Contains(containerName, pauseContainer.Name))
				createStartEventsReported.Add(1)
				go func() {
					eventStream <- createDockerEvent(api.ContainerCreated)
					createStartEventsReported.Done()
				}()
			}).Return(DockerContainerMetadata{DockerID: containerID + ":" + pauseContainer.Name}),
		// Ensure that the pause container is started after it's created
		client.EXPECT().StartContainer(containerID+":"+pauseContainer.Name, startContainerTimeout).Do(
			func(id string, timeout time.Duration) {
				createStartEventsReported.Add(1)
				go func() {
					eventStream <- createDockerEvent(api.ContainerRunning)
					createStartEventsReported.Done()
				}()
			}).Return(DockerContainerMetadata{DockerID: containerID + ":" + pauseContainer.Name}),
		client.EXPECT().InspectContainer(gomock.Any(), gomock.Any()).Return(&docker.Container{
			ID:    containerID,
			State: docker.State{Pid: 23},
		}, nil),
		// Then setting up the pause container network namespace
		mockCNIClient.EXPECT().SetupNS(gomock.Any()).Return(nil),

		// Once the pause container is started, sleep container will be created
		client.EXPECT().CreateContainer(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Do(
			func(config *docker.Config, hostConfig *docker.HostConfig, containerName string, z time.Duration) {
				assert.True(t, strings.Contains(containerName, sleepContainer.Name))
				assert.Equal(t, "container:"+containerID+":"+pauseContainer.Name, hostConfig.NetworkMode)
				createStartEventsReported.Add(1)
				go func() {
					eventStream <- createDockerEvent(api.ContainerCreated)
					createStartEventsReported.Done()
				}()
			}).Return(DockerContainerMetadata{DockerID: containerID + ":" + sleepContainer.Name}),
		// Next, the sleep container is started
		client.EXPECT().StartContainer(containerID+":"+sleepContainer.Name, startContainerTimeout).Do(
			func(id string, timeout time.Duration) {
				createStartEventsReported.Add(1)
				go func() {
					eventStream <- createDockerEvent(api.ContainerRunning)
					createStartEventsReported.Done()
				}()
			}).Return(DockerContainerMetadata{DockerID: containerID + ":" + sleepContainer.Name}),
	)

	// steadyStateCheckWait is used to force the test to wait until the steady-state check
	// has been invoked at least once
	steadyStateCheckWait := sync.WaitGroup{}
	steadyStateVerify := make(chan time.Time, 1)

	mockTime.EXPECT().Now().Do(func() time.Time { return time.Now() }).AnyTimes()
	gomock.InOrder(
		mockTime.EXPECT().After(steadyStateTaskVerifyInterval).Do(func(d time.Duration) {
			steadyStateCheckWait.Done()
		}).Return(steadyStateVerify),
		mockTime.EXPECT().After(steadyStateTaskVerifyInterval).Return(steadyStateVerify).AnyTimes(),
	)
	ctx, cancel := context.WithCancel(context.TODO())
	err := taskEngine.Init(ctx)
	assert.NoError(t, err)
	defer cancel()

	stateChangeEvents := taskEngine.StateChangeEvents()
	steadyStateCheckWait.Add(1)
	taskEngine.AddTask(sleepTask)

	event := <-stateChangeEvents
	assert.Equal(t, event.(api.ContainerStateChange).Status, api.ContainerRunning, "Expected container to be RUNNING")

	event = <-stateChangeEvents
	assert.Equal(t, event.(api.TaskStateChange).Status, api.TaskRunning, "Expected task to be RUNNING")
	select {
	case <-stateChangeEvents:
		t.Fatal("Should be out of events")
	default:
	}

	// Wait for container create and start events to be processed
	createStartEventsReported.Wait()
	// Wait for steady state check to be invoked
	steadyStateCheckWait.Wait()

	cleanup := make(chan time.Time, 1)
	mockTime.EXPECT().After(gomock.Any()).Return(cleanup).AnyTimes()
	client.EXPECT().InspectContainer(gomock.Any(), gomock.Any()).Return(&docker.Container{
		ID:    containerID,
		State: docker.State{Pid: 23},
	}, nil)
	mockCNIClient.EXPECT().CleanupNS(gomock.Any()).Return(nil)
	client.EXPECT().StopContainer(containerID+":"+pauseContainer.Name, gomock.Any()).MinTimes(1)
	mockCNIClient.EXPECT().ReleaseIPResource(gomock.Any()).Return(nil).MaxTimes(1)

	exitCode := 0
	// And then docker reports that sleep died, as sleep is wont to do
	eventStream <- DockerContainerChangeEvent{
		Status: api.ContainerStopped,
		DockerContainerMetadata: DockerContainerMetadata{
			DockerID: containerID + ":" + sleepContainer.Name,
			ExitCode: &exitCode,
		},
	}

	if event := <-stateChangeEvents; event.(api.ContainerStateChange).Status != api.ContainerStopped {
		t.Fatal("Expected container to stop first")
		assert.Equal(t, *event.(api.ContainerStateChange).ExitCode, 0, "Exit code should be present")
	}

	event = <-stateChangeEvents
	assert.Equal(t, event.(api.TaskStateChange).Status, api.TaskStopped, "Task is not in STOPPED state")
}

// TestRemoveEvents tests if the task engine can handle task events while the task is being
// cleaned up. This test ensures that there's no regression in the task engine and ensures
// there's no deadlock as seen in #313
func TestRemoveEvents(t *testing.T) {
	ctrl, client, mockTime, taskEngine, _, imageManager := mocks(t, &defaultConfig)
	defer ctrl.Finish()

	sleepTask := testdata.LoadTask("sleep5")
	eventStream := make(chan DockerContainerChangeEvent)

	// createStartEventsReported is used to force the test to wait until the container created and started
	// events are processed
	createStartEventsReported := sync.WaitGroup{}
	client.EXPECT().Version()
	client.EXPECT().ContainerEvents(gomock.Any()).Return(eventStream, nil)
	var createdContainerName string
	for _, container := range sleepTask.Containers {
		imageManager.EXPECT().AddAllImageStates(gomock.Any()).AnyTimes()
		client.EXPECT().PullImage(container.Image, nil).Return(DockerContainerMetadata{})
		imageManager.EXPECT().RecordContainerReference(container).Return(nil)
		imageManager.EXPECT().GetImageStateFromImageName(gomock.Any()).Return(nil)
		client.EXPECT().CreateContainer(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Do(
			func(config *docker.Config, y interface{}, containerName string, z time.Duration) {
				createdContainerName = containerName
				createStartEventsReported.Add(1)
				go func() {
					eventStream <- createDockerEvent(api.ContainerCreated)
					createStartEventsReported.Done()
				}()
			}).Return(DockerContainerMetadata{DockerID: containerID})

		client.EXPECT().StartContainer(containerID, startContainerTimeout).Do(
			func(id string, timeout time.Duration) {
				createStartEventsReported.Add(1)
				go func() {
					eventStream <- createDockerEvent(api.ContainerRunning)
					createStartEventsReported.Done()
				}()
			}).Return(DockerContainerMetadata{DockerID: containerID})
	}

	// steadyStateCheckWait is used to force the test to wait until the steady-state check
	// has been invoked at least once
	steadyStateCheckWait := sync.WaitGroup{}
	steadyStateVerify := make(chan time.Time, 1)
	cleanup := make(chan time.Time, 1)
	mockTime.EXPECT().Now().Do(func() time.Time { return time.Now() }).AnyTimes()
	gomock.InOrder(
		mockTime.EXPECT().After(steadyStateTaskVerifyInterval).Do(func(d time.Duration) {
			steadyStateCheckWait.Done()
		}).Return(steadyStateVerify),
		mockTime.EXPECT().After(steadyStateTaskVerifyInterval).Return(steadyStateVerify).AnyTimes(),
	)

	ctx, cancel := context.WithCancel(context.TODO())
	err := taskEngine.Init(ctx)
	assert.NoError(t, err)
	defer cancel()

	stateChangeEvents := taskEngine.StateChangeEvents()
	steadyStateCheckWait.Add(1)
	taskEngine.AddTask(sleepTask)

	event := <-stateChangeEvents
	assert.Equal(t, event.(api.ContainerStateChange).Status, api.ContainerRunning, "Expected container to be RUNNING")

	event = <-stateChangeEvents
	assert.Equal(t, event.(api.TaskStateChange).Status, api.TaskRunning, "Expected task to be RUNNING")

	select {
	case <-stateChangeEvents:
		t.Fatal("Should be out of events")
	default:
	}

	// Wait for container create and start events to be processed
	createStartEventsReported.Wait()
	// Wait for steady state check to be invoked
	steadyStateCheckWait.Wait()
	mockTime.EXPECT().After(gomock.Any()).Return(cleanup).AnyTimes()
	client.EXPECT().DescribeContainer(gomock.Any()).AnyTimes()

	// Wait for all events to be consumed prior to moving it towards stopped; we
	// don't want to race the below with these or we'll end up with the "going
	// backwards in state" stop and we haven't 'expect'd for that

	exitCode := 0
	// And then docker reports that sleep died, as sleep is wont to do
	eventStream <- DockerContainerChangeEvent{
		Status: api.ContainerStopped,
		DockerContainerMetadata: DockerContainerMetadata{
			DockerID: containerID,
			ExitCode: &exitCode,
		},
	}

	event = <-stateChangeEvents
	if cont := event.(api.ContainerStateChange); cont.Status != api.ContainerStopped {
		t.Fatal("Expected container to stop first")
		assert.Equal(t, *cont.ExitCode, 0, "Exit code should be present")
	}

	event = <-stateChangeEvents
	assert.Equal(t, event.(api.TaskStateChange).Status, api.TaskStopped, "Expected task to be STOPPED")

	sleepTaskStop := testdata.LoadTask("sleep5")
	sleepTaskStop.SetDesiredStatus(api.TaskStopped)
	taskEngine.AddTask(sleepTaskStop)

	client.EXPECT().RemoveContainer(gomock.Any(), gomock.Any()).Do(
		func(removedContainerName string, timeout time.Duration) {
			assert.Equal(t, createdContainerName, removedContainerName, "Container name mismatch")

			// Emit a couple of events for the task before cleanup finishes. This forces
			// discardEventsUntil to be invoked and should test the code path that
			// caused the deadlock, which was fixed with #320
			eventStream <- createDockerEvent(api.ContainerStopped)
			eventStream <- createDockerEvent(api.ContainerStopped)
		}).Return(nil)

	imageManager.EXPECT().RemoveContainerReferenceFromImageState(gomock.Any())

	// This ensures that managedTask.waitForStopReported makes progress
	sleepTask.SetSentStatus(api.TaskStopped)

	// trigger cleanup
	cleanup <- time.Now()

	// Wait for the task to actually be dead; if we just fallthrough immediately,
	// the remove might not have happened (expectation failure)
	for {
		tasks, _ := taskEngine.(*DockerTaskEngine).ListTasks()
		if len(tasks) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStartTimeoutThenStart(t *testing.T) {
	ctrl, client, testTime, taskEngine, _, imageManager := mocks(t, &defaultConfig)
	defer ctrl.Finish()

	sleepTask := testdata.LoadTask("sleep5")

	eventStream := make(chan DockerContainerChangeEvent)
	testTime.EXPECT().After(gomock.Any())

	client.EXPECT().Version()
	client.EXPECT().ContainerEvents(gomock.Any()).Return(eventStream, nil)
	for _, container := range sleepTask.Containers {
		imageManager.EXPECT().AddAllImageStates(gomock.Any()).AnyTimes()
		client.EXPECT().PullImage(container.Image, nil).Return(DockerContainerMetadata{})

		imageManager.EXPECT().RecordContainerReference(container)
		imageManager.EXPECT().GetImageStateFromImageName(gomock.Any()).Return(nil)
		dockerConfig, err := sleepTask.DockerConfig(container)
		if err != nil {
			t.Fatal(err)
		}

		// Container config should get updated with this during CreateContainer
		dockerConfig.Labels["com.amazonaws.ecs.task-arn"] = sleepTask.Arn
		dockerConfig.Labels["com.amazonaws.ecs.container-name"] = container.Name
		dockerConfig.Labels["com.amazonaws.ecs.task-definition-family"] = sleepTask.Family
		dockerConfig.Labels["com.amazonaws.ecs.task-definition-version"] = sleepTask.Version
		dockerConfig.Labels["com.amazonaws.ecs.cluster"] = ""

		client.EXPECT().CreateContainer(dockerConfig, gomock.Any(), gomock.Any(), gomock.Any()).Do(
			func(x, y, z, timeout interface{}) {
				go func() { eventStream <- createDockerEvent(api.ContainerCreated) }()
			}).Return(DockerContainerMetadata{DockerID: containerID})

		client.EXPECT().StartContainer(containerID, startContainerTimeout).Return(DockerContainerMetadata{
			Error: &DockerTimeoutError{},
		})
	}

	ctx, cancel := context.WithCancel(context.TODO())
	err := taskEngine.Init(ctx)
	assert.NoError(t, err)
	defer cancel()

	stateChangeEvents := taskEngine.StateChangeEvents()
	taskEngine.AddTask(sleepTask)

	// Expect it to go to stopped
	event := <-stateChangeEvents
	assert.Equal(t, event.(api.ContainerStateChange).Status, api.ContainerStopped, "Expected container to timeout on start and stop")

	event = <-stateChangeEvents
	assert.Equal(t, event.(api.TaskStateChange).Status, api.TaskStopped, "Expected task to be STOPPED")

	select {
	case <-stateChangeEvents:
		t.Fatal("Should be out of events")
	default:
	}

	// Expect it to try to stop it once now
	client.EXPECT().StopContainer(containerID, gomock.Any()).Return(DockerContainerMetadata{
		Error: CannotStartContainerError{fmt.Errorf("cannot start container")},
	}).AnyTimes()
	// Now surprise surprise, it actually did start!
	eventStream <- createDockerEvent(api.ContainerRunning)

	// However, if it starts again, we should not see it be killed; no additional expect
	eventStream <- createDockerEvent(api.ContainerRunning)
	eventStream <- createDockerEvent(api.ContainerRunning)

	select {
	case <-stateChangeEvents:
		t.Fatal("Should be out of events")
	default:
	}
}

func TestSteadyStatePoll(t *testing.T) {
	ctrl, client, testTime, taskEngine, _, imageManager := mocks(t, &defaultConfig)
	defer ctrl.Finish()

	wait := &sync.WaitGroup{}
	sleepTask := testdata.LoadTask("sleep5")

	eventStream := make(chan DockerContainerChangeEvent)

	client.EXPECT().Version()
	client.EXPECT().ContainerEvents(gomock.Any()).Return(eventStream, nil)
	// set up expectations for each container in the task calling create + start
	for _, container := range sleepTask.Containers {
		imageManager.EXPECT().AddAllImageStates(gomock.Any()).AnyTimes()
		client.EXPECT().PullImage(container.Image, nil).Return(DockerContainerMetadata{})
		imageManager.EXPECT().RecordContainerReference(container)
		imageManager.EXPECT().GetImageStateFromImageName(gomock.Any()).Return(nil)
		dockerConfig, err := sleepTask.DockerConfig(container)
		assert.Nil(t, err)

		// Container config should get updated with this during CreateContainer
		dockerConfig.Labels["com.amazonaws.ecs.task-arn"] = sleepTask.Arn
		dockerConfig.Labels["com.amazonaws.ecs.container-name"] = container.Name
		dockerConfig.Labels["com.amazonaws.ecs.task-definition-family"] = sleepTask.Family
		dockerConfig.Labels["com.amazonaws.ecs.task-definition-version"] = sleepTask.Version
		dockerConfig.Labels["com.amazonaws.ecs.cluster"] = ""

		wait.Add(1)
		client.EXPECT().CreateContainer(dockerConfig, gomock.Any(), gomock.Any(), gomock.Any()).Do(
			func(x, y, z, timeout interface{}) {
				go func() {
					eventStream <- createDockerEvent(api.ContainerCreated)
					wait.Done()
				}()
			}).Return(DockerContainerMetadata{DockerID: containerID})

		wait.Add(1)
		client.EXPECT().StartContainer(containerID, startContainerTimeout).Do(
			func(id string, timeout time.Duration) {
				go func() {
					eventStream <- createDockerEvent(api.ContainerRunning)
					wait.Done()
				}()
			}).Return(DockerContainerMetadata{DockerID: containerID})
	}

	steadyStateVerify := make(chan time.Time, 10) // channel to trigger a "steady state verify" action
	testTime.EXPECT().After(steadyStateTaskVerifyInterval).Return(steadyStateVerify).AnyTimes()

	ctx, cancel := context.WithCancel(context.TODO())
	err := taskEngine.Init(ctx) // start the task engine
	assert.NoError(t, err)
	defer cancel()

	stateChangeEvents := taskEngine.StateChangeEvents()

	taskEngine.AddTask(sleepTask) // actually add the task we created

	// verify that we get events for the container and task starting, but no other events
	event := <-stateChangeEvents
	assert.Equal(t, event.(api.ContainerStateChange).Status, api.ContainerRunning, "Expected container to be RUNNING")

	event = <-stateChangeEvents
	assert.Equal(t, event.(api.TaskStateChange).Status, api.TaskRunning, "Expected task to be RUNNING")

	select {
	case <-stateChangeEvents:
		t.Fatal("Should be out of events")
	default:
	}

	containerMap, ok := taskEngine.(*DockerTaskEngine).State().ContainerMapByArn(sleepTask.Arn)
	assert.True(t, ok)
	dockerContainer, ok := containerMap[sleepTask.Containers[0].Name]
	assert.True(t, ok)

	// Two steady state oks, one stop
	gomock.InOrder(
		client.EXPECT().DescribeContainer(containerID).Return(
			api.ContainerRunning,
			DockerContainerMetadata{
				DockerID: containerID,
			}).Times(2),
		client.EXPECT().DescribeContainer(containerID).Return(
			api.ContainerStopped,
			DockerContainerMetadata{
				DockerID: containerID,
			}).MinTimes(1),
		// the engine *may* call StopContainer even though it's already stopped
		client.EXPECT().StopContainer(containerID, stopContainerTimeout).AnyTimes(),
	)
	wait.Wait()

	cleanupChan := make(chan time.Time)
	testTime.EXPECT().After(gomock.Any()).Return(cleanupChan).AnyTimes()
	client.EXPECT().RemoveContainer(dockerContainer.DockerName, removeContainerTimeout).Return(nil)
	imageManager.EXPECT().RemoveContainerReferenceFromImageState(gomock.Any()).Return(nil)

	// trigger steady state verification
	for i := 0; i < 10; i++ {
		steadyStateVerify <- time.Now()
	}

	event = <-stateChangeEvents
	assert.Equal(t, event.(api.ContainerStateChange).Status, api.ContainerStopped, "Expected container to be STOPPED")

	event = <-stateChangeEvents
	assert.Equal(t, event.(api.TaskStateChange).Status, api.TaskStopped, "Expected task to be STOPPED")

	select {
	case <-stateChangeEvents:
		t.Fatal("Should be out of events")
	default:
	}

	close(steadyStateVerify)
	// trigger cleanup, this ensures all the goroutines were finished
	sleepTask.SetSentStatus(api.TaskStopped)
	cleanupChan <- time.Now()

	for {
		tasks, _ := taskEngine.(*DockerTaskEngine).ListTasks()
		if len(tasks) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStopWithPendingStops(t *testing.T) {
	ctrl, client, testTime, taskEngine, _, imageManager := mocks(t, &defaultConfig)
	defer ctrl.Finish()
	testTime.EXPECT().Now().AnyTimes()
	testTime.EXPECT().After(gomock.Any()).AnyTimes()

	sleepTask1 := testdata.LoadTask("sleep5")
	sleepTask1.StartSequenceNumber = 5
	sleepTask2 := testdata.LoadTask("sleep5")
	sleepTask2.Arn = "arn2"

	eventStream := make(chan DockerContainerChangeEvent)

	client.EXPECT().Version()
	client.EXPECT().ContainerEvents(gomock.Any()).Return(eventStream, nil)
	ctx, cancel := context.WithCancel(context.TODO())
	err := taskEngine.Init(ctx)
	assert.NoError(t, err)
	defer cancel()
	stateChangeEvents := taskEngine.StateChangeEvents()
	go func() {
		for {
			<-stateChangeEvents
		}
	}()

	pullDone := make(chan bool)
	pullInvoked := make(chan bool)
	client.EXPECT().PullImage(gomock.Any(), nil).Do(func(x, y interface{}) {
		pullInvoked <- true
		<-pullDone
	})

	imageManager.EXPECT().RecordContainerReference(gomock.Any()).AnyTimes()
	imageManager.EXPECT().GetImageStateFromImageName(gomock.Any()).AnyTimes()

	taskEngine.AddTask(sleepTask2)
	<-pullInvoked
	stopSleep2 := testdata.LoadTask("sleep5")
	stopSleep2.Arn = "arn2"
	stopSleep2.SetDesiredStatus(api.TaskStopped)
	stopSleep2.StopSequenceNumber = 4
	taskEngine.AddTask(stopSleep2)

	taskEngine.AddTask(sleepTask1)
	stopSleep1 := testdata.LoadTask("sleep5")
	stopSleep1.SetDesiredStatus(api.TaskStopped)
	stopSleep1.StopSequenceNumber = 5
	taskEngine.AddTask(stopSleep1)
	pullDone <- true
	// this means the PullImage is only called once due to the task is stopped before it
	// gets the pull image lock
}

func TestCreateContainerForceSave(t *testing.T) {
	ctrl, client, _, privateTaskEngine, _, _ := mocks(t, &config.Config{})
	saver := mock_statemanager.NewMockStateManager(ctrl)
	defer ctrl.Finish()
	taskEngine, _ := privateTaskEngine.(*DockerTaskEngine)
	taskEngine.SetSaver(saver)

	sleepTask := testdata.LoadTask("sleep5")
	sleepContainer, _ := sleepTask.ContainerByName("sleep5")

	gomock.InOrder(
		saver.EXPECT().ForceSave().Do(func() interface{} {
			task, ok := taskEngine.state.TaskByArn(sleepTask.Arn)
			assert.True(t, ok, "Expected task with ARN: ", sleepTask.Arn)
			assert.NotNil(t, task, "Expected task with ARN: ", sleepTask.Arn)
			_, ok = task.ContainerByName("sleep5")
			assert.True(t, ok, "Expected container sleep5")
			return nil
		}),
		client.EXPECT().CreateContainer(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()),
	)

	metadata := taskEngine.createContainer(sleepTask, sleepContainer)
	if metadata.Error != nil {
		t.Error("Unexpected error", metadata.Error)
	}
}

func TestCreateContainerMergesLabels(t *testing.T) {
	ctrl, client, _, taskEngine, _, _ := mocks(t, &defaultConfig)
	defer ctrl.Finish()

	testTask := &api.Task{
		Arn:     "arn:aws:ecs:us-east-1:012345678910:task/c09f0188-7f87-4b0f-bfc3-16296622b6fe",
		Family:  "myFamily",
		Version: "1",
		Containers: []*api.Container{
			{
				Name: "c1",
				DockerConfig: api.DockerConfig{
					Config: aws.String(`{"Labels":{"key":"value"}}`),
				},
			},
		},
	}
	expectedConfig, err := testTask.DockerConfig(testTask.Containers[0])
	if err != nil {
		t.Fatal(err)
	}
	expectedConfig.Labels = map[string]string{
		"com.amazonaws.ecs.task-arn":                "arn:aws:ecs:us-east-1:012345678910:task/c09f0188-7f87-4b0f-bfc3-16296622b6fe",
		"com.amazonaws.ecs.container-name":          "c1",
		"com.amazonaws.ecs.task-definition-family":  "myFamily",
		"com.amazonaws.ecs.task-definition-version": "1",
		"com.amazonaws.ecs.cluster":                 "",
		"key": "value",
	}
	client.EXPECT().CreateContainer(expectedConfig, gomock.Any(), gomock.Any(), gomock.Any())
	taskEngine.(*DockerTaskEngine).createContainer(testTask, testTask.Containers[0])
}

// TestTaskTransitionWhenStopContainerTimesout tests that task transitions to stopped
// only when terminal events are recieved from docker event stream when
// StopContainer times out
func TestTaskTransitionWhenStopContainerTimesout(t *testing.T) {
	ctrl, client, mockTime, taskEngine, _, imageManager := mocks(t, &defaultConfig)
	defer ctrl.Finish()

	sleepTask := testdata.LoadTask("sleep5")

	eventStream := make(chan DockerContainerChangeEvent)

	client.EXPECT().Version()
	client.EXPECT().ContainerEvents(gomock.Any()).Return(eventStream, nil)
	mockTime.EXPECT().After(gomock.Any()).AnyTimes()
	containerStopTimeoutError := DockerContainerMetadata{
		Error: &DockerTimeoutError{
			transition: "stop",
			duration:   stopContainerTimeout,
		},
	}
	dockerEventSent := make(chan int)
	for _, container := range sleepTask.Containers {
		imageManager.EXPECT().AddAllImageStates(gomock.Any()).AnyTimes()
		client.EXPECT().PullImage(container.Image, nil).Return(DockerContainerMetadata{})
		imageManager.EXPECT().RecordContainerReference(container)
		imageManager.EXPECT().GetImageStateFromImageName(gomock.Any()).Return(nil)
		dockerConfig, err := sleepTask.DockerConfig(container)
		if err != nil {
			t.Fatal(err)
		}
		// Container config should get updated with this during CreateContainer
		dockerConfig.Labels["com.amazonaws.ecs.task-arn"] = sleepTask.Arn
		dockerConfig.Labels["com.amazonaws.ecs.container-name"] = container.Name
		dockerConfig.Labels["com.amazonaws.ecs.task-definition-family"] = sleepTask.Family
		dockerConfig.Labels["com.amazonaws.ecs.task-definition-version"] = sleepTask.Version
		dockerConfig.Labels["com.amazonaws.ecs.cluster"] = ""

		client.EXPECT().CreateContainer(dockerConfig, gomock.Any(), gomock.Any(), gomock.Any()).Do(
			func(x, y, z, timeout interface{}) {
				go func() { eventStream <- createDockerEvent(api.ContainerCreated) }()
			}).Return(DockerContainerMetadata{DockerID: containerID})

		gomock.InOrder(
			client.EXPECT().StartContainer(containerID, startContainerTimeout).Do(
				func(id string, timeout time.Duration) {
					go func() {
						eventStream <- createDockerEvent(api.ContainerRunning)
					}()
				}).Return(DockerContainerMetadata{DockerID: containerID}),

			// StopContainer times out
			client.EXPECT().StopContainer(containerID, gomock.Any()).Return(containerStopTimeoutError),
			// Since task is not in steady state, progressContainers causes
			// another invocation of StopContainer. Return a timeout error
			// for that as well.
			client.EXPECT().StopContainer(containerID, gomock.Any()).Do(
				func(id string, timeout time.Duration) {
					go func() {
						dockerEventSent <- 1
						// Emit 'ContainerStopped' event to the container event stream
						// This should cause the container and the task to transition
						// to 'STOPPED'
						eventStream <- createDockerEvent(api.ContainerStopped)
					}()
				}).Return(containerStopTimeoutError).MinTimes(1),
		)
	}

	ctx, cancel := context.WithCancel(context.TODO())
	err := taskEngine.Init(ctx)
	assert.NoError(t, err)
	defer cancel()
	stateChangeEvents := taskEngine.StateChangeEvents()

	go taskEngine.AddTask(sleepTask)
	// wait for task running
	event := <-stateChangeEvents
	assert.Equal(t, event.(api.ContainerStateChange).Status, api.ContainerRunning, "Expected container to be RUNNING")

	event = <-stateChangeEvents
	assert.Equal(t, event.(api.TaskStateChange).Status, api.TaskRunning, "Expected task to be RUNNING")

	// Set the task desired status to be stopped and StopContainer will be called
	updateSleepTask := testdata.LoadTask("sleep5")
	updateSleepTask.SetDesiredStatus(api.TaskStopped)
	go taskEngine.AddTask(updateSleepTask)

	// StopContainer timeout error shouldn't cause cantainer/task status change
	// until receive stop event from docker event stream
	select {
	case <-stateChangeEvents:
		t.Error("Should not get task events")
	case <-dockerEventSent:
		t.Logf("Send docker stop event")
		go func() {
			for {
				<-dockerEventSent
			}
		}()
	}

	// StopContainer was called again and received stop event from docker event stream
	// Expect it to go to stopped
	event = <-stateChangeEvents
	assert.Equal(t, event.(api.ContainerStateChange).Status, api.ContainerStopped, "Expected container to timeout on start and stop")

	event = <-stateChangeEvents
	assert.Equal(t, event.(api.TaskStateChange).Status, api.TaskStopped, "Expected task to be STOPPED")

	select {
	case <-stateChangeEvents:
		t.Error("Should be out of events")
	default:
	}
}

// TestTaskTransitionWhenStopContainerReturnsUnretriableError tests if the task transitions
// to stopped without retrying stopping the container in the task when the initial
// stop container call returns an unretriable error from docker, specifically the
// ContainerNotRunning error
func TestTaskTransitionWhenStopContainerReturnsUnretriableError(t *testing.T) {
	ctrl, client, mockTime, taskEngine, _, imageManager := mocks(t, &defaultConfig)
	defer ctrl.Finish()

	sleepTask := testdata.LoadTask("sleep5")
	eventStream := make(chan DockerContainerChangeEvent)
	client.EXPECT().Version()
	client.EXPECT().ContainerEvents(gomock.Any()).Return(eventStream, nil)
	mockTime.EXPECT().After(gomock.Any()).AnyTimes()
	eventsReported := sync.WaitGroup{}
	for _, container := range sleepTask.Containers {
		gomock.InOrder(
			imageManager.EXPECT().AddAllImageStates(gomock.Any()).AnyTimes(),
			client.EXPECT().PullImage(container.Image, nil).Return(DockerContainerMetadata{}),
			imageManager.EXPECT().RecordContainerReference(container),
			imageManager.EXPECT().GetImageStateFromImageName(gomock.Any()).Return(nil),
			// Simulate successful create container
			client.EXPECT().CreateContainer(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Do(
				func(x, y, z, timeout interface{}) {
					eventsReported.Add(1)
					go func() {
						eventStream <- createDockerEvent(api.ContainerCreated)
						eventsReported.Done()
					}()
				}).Return(DockerContainerMetadata{DockerID: containerID}),
			// Simulate successful start container
			client.EXPECT().StartContainer(containerID, startContainerTimeout).Do(
				func(id string, timeout time.Duration) {
					eventsReported.Add(1)
					go func() {
						eventStream <- createDockerEvent(api.ContainerRunning)
						eventsReported.Done()
					}()
				}).Return(DockerContainerMetadata{DockerID: containerID}),
			// StopContainer errors out. However, since this is a known unretriable error,
			// the task engine should not retry stopping the container and move on.
			// If there's a delay in task engine's processing of the ContainerRunning
			// event, StopContainer will be invoked again as the engine considers it
			// as a stopped container coming back. MinTimes() should guarantee that
			// StopContainer is invoked at least once and in protecting agasint a test
			// failure when there's a delay in task engine processing the ContainerRunning
			// event.
			client.EXPECT().StopContainer(containerID, gomock.Any()).Return(DockerContainerMetadata{
				Error: CannotStopContainerError{&docker.ContainerNotRunning{}},
			}).MinTimes(1),
		)
	}

	ctx, cancel := context.WithCancel(context.TODO())
	err := taskEngine.Init(ctx)
	assert.NoError(t, err)
	defer cancel()
	stateChangeEvents := taskEngine.StateChangeEvents()

	go taskEngine.AddTask(sleepTask)
	// wait for task running
	event := <-stateChangeEvents
	assert.Equal(t, event.(api.ContainerStateChange).Status, api.ContainerRunning, "Expected container to be RUNNING")

	event = <-stateChangeEvents
	assert.Equal(t, event.(api.TaskStateChange).Status, api.TaskRunning, "Expected task to be RUNNING")

	select {
	case <-stateChangeEvents:
		t.Fatal("Should be out of events")
	default:
	}
	eventsReported.Wait()

	// Set the task desired status to be stopped and StopContainer will be called
	updateSleepTask := testdata.LoadTask("sleep5")
	updateSleepTask.SetDesiredStatus(api.TaskStopped)
	go taskEngine.AddTask(updateSleepTask)

	// StopContainer was called again and received stop event from docker event stream
	// Expect it to go to stopped
	event = <-stateChangeEvents
	assert.Equal(t, event.(api.ContainerStateChange).Status, api.ContainerStopped, "Expected container to be STOPPED")

	event = <-stateChangeEvents
	assert.Equal(t, event.(api.TaskStateChange).Status, api.TaskStopped, "Expected task to be STOPPED")

	select {
	case <-stateChangeEvents:
		t.Error("Should be out of events")
	default:
	}
}

// TestTaskTransitionWhenStopContainerReturnsTransientErrorBeforeSucceeding tests if the task
// transitions to stopped only after receiving the container stopped event from docker when
// the initial stop container call fails with an unknown error.
func TestTaskTransitionWhenStopContainerReturnsTransientErrorBeforeSucceeding(t *testing.T) {
	ctrl, client, mockTime, taskEngine, _, imageManager := mocks(t, &defaultConfig)
	defer ctrl.Finish()

	sleepTask := testdata.LoadTask("sleep5")
	eventStream := make(chan DockerContainerChangeEvent)

	client.EXPECT().Version()
	client.EXPECT().ContainerEvents(gomock.Any()).Return(eventStream, nil)
	mockTime.EXPECT().After(gomock.Any()).AnyTimes()
	containerStoppingError := DockerContainerMetadata{
		Error: CannotStopContainerError{errors.New("Error stopping container")},
	}
	for _, container := range sleepTask.Containers {
		gomock.InOrder(
			imageManager.EXPECT().AddAllImageStates(gomock.Any()).AnyTimes(),
			client.EXPECT().PullImage(container.Image, nil).Return(DockerContainerMetadata{}),
			imageManager.EXPECT().RecordContainerReference(container),
			imageManager.EXPECT().GetImageStateFromImageName(gomock.Any()).Return(nil),
			// Simulate successful create container
			client.EXPECT().CreateContainer(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(
				DockerContainerMetadata{DockerID: containerID}),
			// Simulate successful start container
			client.EXPECT().StartContainer(containerID, startContainerTimeout).Return(
				DockerContainerMetadata{DockerID: containerID}),
			// StopContainer errors out a couple of times
			client.EXPECT().StopContainer(containerID, gomock.Any()).Return(containerStoppingError).Times(2),
			// Since task is not in steady state, progressContainers causes
			// another invocation of StopContainer. Return the 'succeed' response,
			// which should cause the task engine to stop invoking this again and
			// transition the task to stopped.
			client.EXPECT().StopContainer(containerID, gomock.Any()).Return(DockerContainerMetadata{}),
		)
	}

	ctx, cancel := context.WithCancel(context.TODO())
	err := taskEngine.Init(ctx)
	assert.NoError(t, err)
	defer cancel()

	stateChangeEvents := taskEngine.StateChangeEvents()

	go taskEngine.AddTask(sleepTask)
	// wait for task running

	event := <-stateChangeEvents
	assert.Equal(t, event.(api.ContainerStateChange).Status, api.ContainerRunning, "Expected container to be RUNNING")

	event = <-stateChangeEvents
	assert.Equal(t, event.(api.TaskStateChange).Status, api.TaskRunning, "Expected task to be RUNNING")

	select {
	case <-stateChangeEvents:
		t.Fatal("Should be out of events")
	default:
	}

	// Set the task desired status to be stopped and StopContainer will be called
	updateSleepTask := testdata.LoadTask("sleep5")
	updateSleepTask.SetDesiredStatus(api.TaskStopped)
	go taskEngine.AddTask(updateSleepTask)

	// StopContainer invocation should have caused it to stop eventually.
	event = <-stateChangeEvents
	assert.Equal(t, event.(api.ContainerStateChange).Status, api.ContainerStopped, "Expected container to be STOPPED")

	event = <-stateChangeEvents
	assert.Equal(t, event.(api.TaskStateChange).Status, api.TaskStopped, "Expected task to be STOPPED")

	select {
	case <-stateChangeEvents:
		t.Error("Should be out of events")
	default:
	}
}

func TestGetTaskByArn(t *testing.T) {
	// Need a mock client as AddTask not only adds a task to the engine, but
	// also causes the engine to progress the task.

	ctrl, client, _, taskEngine, _, imageManager := mocks(t, &defaultConfig)
	defer ctrl.Finish()

	client.EXPECT().Version()
	eventStream := make(chan DockerContainerChangeEvent)
	client.EXPECT().ContainerEvents(gomock.Any()).Return(eventStream, nil)
	imageManager.EXPECT().AddAllImageStates(gomock.Any()).AnyTimes()
	imageManager.EXPECT().RecordContainerReference(gomock.Any()).AnyTimes()
	imageManager.EXPECT().GetImageStateFromImageName(gomock.Any()).AnyTimes()
	client.EXPECT().PullImage(gomock.Any(), gomock.Any()).AnyTimes() // TODO change to MaxTimes(1)

	ctx, cancel := context.WithCancel(context.TODO())
	err := taskEngine.Init(ctx)
	assert.NoError(t, err)
	defer cancel()
	defer taskEngine.Disable()

	sleepTask := testdata.LoadTask("sleep5")
	sleepTaskArn := sleepTask.Arn
	taskEngine.AddTask(sleepTask)

	_, found := taskEngine.GetTaskByArn(sleepTaskArn)
	assert.True(t, found, "Task %s not found", sleepTaskArn)

	_, found = taskEngine.GetTaskByArn(sleepTaskArn + "arn")
	assert.False(t, found, "Task with invalid arn found in the task engine")
}

func TestEngineEnableConcurrentPull(t *testing.T) {
	ctrl, client, _, taskEngine, _, _ := mocks(t, &defaultConfig)
	defer ctrl.Finish()

	client.EXPECT().Version().Return("1.11.1", nil)
	client.EXPECT().ContainerEvents(gomock.Any())

	ctx, cancel := context.WithCancel(context.TODO())
	err := taskEngine.Init(ctx)
	assert.NoError(t, err)
	defer cancel()

	dockerTaskEngine, _ := taskEngine.(*DockerTaskEngine)
	assert.True(t, dockerTaskEngine.enableConcurrentPull,
		"Task engine should be able to perform concurrent pulling for docker version >= 1.11.1")
}

func TestEngineDisableConcurrentPull(t *testing.T) {
	ctrl, client, _, taskEngine, _, _ := mocks(t, &defaultConfig)
	defer ctrl.Finish()

	client.EXPECT().Version().Return("1.11.0", nil)
	client.EXPECT().ContainerEvents(gomock.Any())

	ctx, cancel := context.WithCancel(context.TODO())
	err := taskEngine.Init(ctx)
	assert.NoError(t, err)
	defer cancel()

	dockerTaskEngine, _ := taskEngine.(*DockerTaskEngine)
	assert.False(t, dockerTaskEngine.enableConcurrentPull,
		"Task engine should not be able to perform concurrent pulling for version < 1.11.1")
}

func TestPauseContainerHappyPath(t *testing.T) {
	ctrl, dockerClient, mockTime, taskEngine, _, imageManager := mocks(t, &defaultConfig)
	defer ctrl.Finish()

	cniClient := mock_ecscni.NewMockCNIClient(ctrl)
	taskEngine.(*DockerTaskEngine).cniClient = cniClient

	eventStream := make(chan DockerContainerChangeEvent)
	sleepTask := testdata.LoadTask("sleep5")

	// Add eni information to the task so the task can add dependency of pause container
	sleepTask.SetTaskENI(&api.ENI{
		ID:         "id",
		MacAddress: "mac",
		IPV4Addresses: []*api.ENIIPV4Address{
			{
				Primary: true,
				Address: "ipv4",
			},
		},
	})

	dockerClient.EXPECT().Version()
	dockerClient.EXPECT().ContainerEvents(gomock.Any()).Return(eventStream, nil)

	pauseContainerID := "pauseContainerID"
	// Pause container will be launched first
	gomock.InOrder(
		dockerClient.EXPECT().CreateContainer(
			gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Do(
			func(config *docker.Config, x, y, z interface{}) {
				name, ok := config.Labels[labelPrefix+"container-name"]
				assert.True(t, ok)
				assert.Equal(t, api.PauseContainerName, name)
			}).Return(DockerContainerMetadata{DockerID: "pauseContainerID"}),
		dockerClient.EXPECT().StartContainer(pauseContainerID, startContainerTimeout).Return(
			DockerContainerMetadata{DockerID: "pauseContainerID"}),
		dockerClient.EXPECT().InspectContainer(gomock.Any(), gomock.Any()).Return(
			&docker.Container{
				ID:    pauseContainerID,
				State: docker.State{Pid: 123},
			}, nil),
		cniClient.EXPECT().SetupNS(gomock.Any()).Return(nil),
	)

	// For the other container
	imageManager.EXPECT().AddAllImageStates(gomock.Any()).AnyTimes()
	dockerClient.EXPECT().PullImage(gomock.Any(), nil).Return(DockerContainerMetadata{})
	imageManager.EXPECT().RecordContainerReference(gomock.Any()).Return(nil)
	imageManager.EXPECT().GetImageStateFromImageName(gomock.Any()).Return(nil)
	dockerClient.EXPECT().CreateContainer(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(DockerContainerMetadata{DockerID: containerID})

	dockerClient.EXPECT().StartContainer(containerID, startContainerTimeout).Return(
		DockerContainerMetadata{DockerID: containerID})

	steadyStateVerify := make(chan time.Time)
	cleanup := make(chan time.Time)
	mockTime.EXPECT().Now().Do(func() time.Time { return time.Now() }).AnyTimes()
	// Expect steady state check once
	mockTime.EXPECT().After(steadyStateTaskVerifyInterval).Return(steadyStateVerify).MinTimes(1)
	dockerClient.EXPECT().DescribeContainer(containerID).AnyTimes()
	dockerClient.EXPECT().DescribeContainer(pauseContainerID).AnyTimes()

	ctx, cancel := context.WithCancel(context.TODO())
	err := taskEngine.Init(ctx)
	assert.NoError(t, err)
	defer cancel()

	taskEngine.AddTask(sleepTask)
	stateChangeEvents := taskEngine.StateChangeEvents()

	verifyTaskIsRunning(stateChangeEvents, sleepTask)
	steadyStateVerify <- time.Now()

	mockTime.EXPECT().After(gomock.Any()).Return(cleanup).AnyTimes()
	dockerClient.EXPECT().InspectContainer(gomock.Any(), gomock.Any()).Return(&docker.Container{
		ID:    pauseContainerID,
		State: docker.State{Pid: 123},
	}, nil)
	cniClient.EXPECT().CleanupNS(gomock.Any()).Return(nil)
	dockerClient.EXPECT().StopContainer(pauseContainerID, gomock.Any()).Return(
		DockerContainerMetadata{DockerID: pauseContainerID})
	cniClient.EXPECT().ReleaseIPResource(gomock.Any()).Return(nil)
	dockerClient.EXPECT().RemoveContainer(gomock.Any(), gomock.Any()).Return(nil).Times(2)
	imageManager.EXPECT().RemoveContainerReferenceFromImageState(gomock.Any()).Return(nil)

	exitCode := 0
	eventStream <- DockerContainerChangeEvent{
		Status: api.ContainerStopped,
		DockerContainerMetadata: DockerContainerMetadata{
			DockerID: containerID,
			ExitCode: &exitCode,
		},
	}

	verifyTaskIsStopped(stateChangeEvents, sleepTask)
	sleepTask.SetSentStatus(api.TaskStopped)
	for {
		go func() { cleanup <- time.Now() }()
		tasks, _ := taskEngine.(*DockerTaskEngine).ListTasks()
		if len(tasks) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBuildCNIConfigFromTaskContainer(t *testing.T) {
	for _, blockIMDS := range []bool{true, false} {
		t.Run(fmt.Sprintf("When BlockInstanceMetadata is %t", blockIMDS), func(t *testing.T) {
			config := defaultConfig
			config.AWSVPCBlockInstanceMetdata = blockIMDS
			ctrl, dockerClient, _, taskEngine, _, _ := mocks(t, &config)
			defer ctrl.Finish()

			testTask := testdata.LoadTask("sleep5")
			testTask.SetTaskENI(&api.ENI{
				ID: "TestBuildCNIConfigFromTaskContainer",
				IPV4Addresses: []*api.ENIIPV4Address{
					{
						Primary: true,
						Address: ipv4,
					},
				},
				MacAddress: mac,
				IPV6Addresses: []*api.ENIIPV6Address{
					{
						Address: ipv6,
					},
				},
			})
			container := &api.Container{
				Name: "container",
			}
			taskEngine.(*DockerTaskEngine).state.AddContainer(&api.DockerContainer{
				Container:  container,
				DockerName: dockerContainerName,
			}, testTask)

			dockerClient.EXPECT().InspectContainer(dockerContainerName, gomock.Any()).Return(&docker.Container{
				ID:    containerID,
				State: docker.State{Pid: containerPid},
			}, nil)

			cniConfig, err := taskEngine.(*DockerTaskEngine).buildCNIConfigFromTaskContainer(testTask, container)
			assert.NoError(t, err)
			assert.Equal(t, containerID, cniConfig.ContainerID)
			assert.Equal(t, strconv.Itoa(containerPid), cniConfig.ContainerPID)
			assert.Equal(t, mac, cniConfig.ID, "ID should be set to the mac of eni")
			assert.Equal(t, mac, cniConfig.ENIMACAddress)
			assert.Equal(t, ipv4, cniConfig.ENIIPV4Address)
			assert.Equal(t, ipv6, cniConfig.ENIIPV6Address)
			assert.Equal(t, blockIMDS, cniConfig.BlockInstanceMetdata)
		})
	}
}

func TestBuildCNIConfigFromTaskContainerInspectError(t *testing.T) {
	ctrl, dockerClient, _, taskEngine, _, _ := mocks(t, &defaultConfig)
	defer ctrl.Finish()

	testTask := testdata.LoadTask("sleep5")
	testTask.SetTaskENI(&api.ENI{})
	container := &api.Container{
		Name: "container",
	}
	taskEngine.(*DockerTaskEngine).state.AddContainer(&api.DockerContainer{
		Container:  container,
		DockerName: dockerContainerName,
	}, testTask)

	dockerClient.EXPECT().InspectContainer(dockerContainerName, gomock.Any()).Return(nil, errors.New("error"))

	_, err := taskEngine.(*DockerTaskEngine).buildCNIConfigFromTaskContainer(testTask, container)
	assert.Error(t, err)
}

// TestStopPauseContainerCleanupCalled tests when stopping the pause container
// its network namespace should be cleaned up first
func TestStopPauseContainerCleanupCalled(t *testing.T) {
	ctrl, dockerClient, _, taskEngine, _, _ := mocks(t, &defaultConfig)
	defer ctrl.Finish()

	mockCNIClient := mock_ecscni.NewMockCNIClient(ctrl)
	taskEngine.(*DockerTaskEngine).cniClient = mockCNIClient

	testTask := testdata.LoadTask("sleep5")

	pauseContainer := &api.Container{
		Name: "pausecontainer",
		Type: api.ContainerCNIPause,
	}
	testTask.Containers = append(testTask.Containers, pauseContainer)

	testTask.SetTaskENI(&api.ENI{
		ID: "TestStopPauseContainerCleanupCalled",
		IPV4Addresses: []*api.ENIIPV4Address{
			{
				Primary: true,
				Address: ipv4,
			},
		},
		MacAddress: mac,
		IPV6Addresses: []*api.ENIIPV6Address{
			{
				Address: ipv6,
			},
		},
	})
	taskEngine.(*DockerTaskEngine).State().AddTask(testTask)
	taskEngine.(*DockerTaskEngine).State().AddContainer(&api.DockerContainer{
		DockerID:   containerID,
		DockerName: dockerContainerName,
		Container:  pauseContainer,
	}, testTask)

	gomock.InOrder(
		dockerClient.EXPECT().InspectContainer(dockerContainerName, gomock.Any()).Return(&docker.Container{
			ID:    containerID,
			State: docker.State{Pid: containerPid},
		}, nil),
		mockCNIClient.EXPECT().CleanupNS(gomock.Any()).Return(nil),
		dockerClient.EXPECT().StopContainer(containerID, stopContainerTimeout).Return(DockerContainerMetadata{}),
	)

	taskEngine.(*DockerTaskEngine).stopContainer(testTask, pauseContainer)
}

// TestTaskWithCircularDependency tests the task with containers of which the
// dependencies can't be resolved
func TestTaskWithCircularDependency(t *testing.T) {
	ctrl, client, _, taskEngine, _, _ := mocks(t, &defaultConfig)
	defer ctrl.Finish()

	client.EXPECT().Version().Return("1.12.6", nil)
	client.EXPECT().ContainerEvents(gomock.Any())

	task := testdata.LoadTask("circular_dependency")

	ctx, cancel := context.WithCancel(context.TODO())
	err := taskEngine.Init(ctx)
	assert.NoError(t, err)
	defer cancel()

	events := taskEngine.StateChangeEvents()
	go taskEngine.AddTask(task)

	event := <-events
	assert.Equal(t, event.(api.TaskStateChange).Status, api.TaskStopped, "Expected task to move to stopped directly")
	_, ok := taskEngine.(*DockerTaskEngine).state.TaskByArn(task.Arn)
	assert.True(t, ok, "Task state should be added to the agent state")

	_, ok = taskEngine.(*DockerTaskEngine).managedTasks[task.Arn]
	assert.False(t, ok, "Task should not be added to task manager for processing")
}

// TestCreateContainerOnAgentRestart tests when agent restarts it should use the
// docker container name restored from agent state file to create the container
func TestCreateContainerOnAgentRestart(t *testing.T) {
	ctrl, client, _, privateTaskEngine, _, _ := mocks(t, &config.Config{})
	saver := mock_statemanager.NewMockStateManager(ctrl)
	defer ctrl.Finish()

	taskEngine, _ := privateTaskEngine.(*DockerTaskEngine)
	taskEngine.SetSaver(saver)
	state := taskEngine.State()

	sleepTask := testdata.LoadTask("sleep5")
	sleepContainer, _ := sleepTask.ContainerByName("sleep5")
	// Store the generated container name to state
	state.AddContainer(&api.DockerContainer{DockerName: "docker_container_name", Container: sleepContainer}, sleepTask)

	gomock.InOrder(
		client.EXPECT().CreateContainer(gomock.Any(), gomock.Any(), "docker_container_name", gomock.Any()),
	)

	metadata := taskEngine.createContainer(sleepTask, sleepContainer)
	if metadata.Error != nil {
		t.Error("Unexpected error", metadata.Error)
	}
}

func TestPullCNIImage(t *testing.T) {
	ctrl, _, _, privateTaskEngine, _, _ := mocks(t, &config.Config{})
	defer ctrl.Finish()
	taskEngine, _ := privateTaskEngine.(*DockerTaskEngine)

	container := &api.Container{
		Type: api.ContainerCNIPause,
	}
	task := &api.Task{
		Containers: []*api.Container{container},
	}
	metadata := taskEngine.pullContainer(task, container)
	assert.Equal(t, DockerContainerMetadata{}, metadata, "expected empty metadata")
}

func TestPullNormalImage(t *testing.T) {
	ctrl, client, _, privateTaskEngine, _, imageManager := mocks(t, &config.Config{})
	defer ctrl.Finish()
	taskEngine, _ := privateTaskEngine.(*DockerTaskEngine)
	saver := mock_statemanager.NewMockStateManager(ctrl)
	taskEngine.SetSaver(saver)

	imageName := "image"
	container := &api.Container{
		Type:  api.ContainerNormal,
		Image: imageName,
	}
	task := &api.Task{
		Containers: []*api.Container{container},
	}
	imageState := &image.ImageState{
		Image: &image.Image{ImageID: "id"},
	}

	client.EXPECT().PullImage(imageName, nil)
	imageManager.EXPECT().RecordContainerReference(container)
	imageManager.EXPECT().GetImageStateFromImageName(imageName).Return(imageState)
	saver.EXPECT().Save()

	metadata := taskEngine.pullContainer(task, container)
	assert.Equal(t, DockerContainerMetadata{}, metadata, "expected empty metadata")
}
