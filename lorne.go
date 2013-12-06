package main

import (
	"flag"
	"io"
	"log"
	"os"
	"strconv"

	"github.com/flynn/go-discover/discover"
	"github.com/flynn/lorne/types"
	sampic "github.com/flynn/sampi/client"
	"github.com/flynn/sampi/types"
	"github.com/technoweenie/grohl"
	"github.com/titanous/go-dockerclient"
)

func main() {
	externalAddr := flag.String("external", "", "external IP of host")
	configFile := flag.String("config", "", "configuration file")
	hostID := flag.String("id", "host1", "host id")
	flag.Parse()
	grohl.AddContext("app", "lorne")
	grohl.Log(grohl.Data{"at": "start"})
	g := grohl.NewContext(grohl.Data{"fn": "main"})

	disc, err := discover.NewClient()
	if err != nil {
		log.Fatal(err)
	}
	if err := disc.Register("flynn-lorne."+*hostID, "1113", nil); err != nil {
		log.Fatal(err)
	}

	scheduler, err := sampic.New()
	if err != nil {
		log.Fatal(err)
	}
	g.Log(grohl.Data{"at": "sampi_connected"})

	dockerc, err := docker.NewClient("http://localhost:4243")
	if err != nil {
		log.Fatal(err)
	}

	state := NewState()
	ports := make(chan int)

	go allocatePorts(ports, 55000, 65535)
	go serveHTTP(&Host{state: state, docker: dockerc}, &attachHandler{state: state, docker: dockerc})
	go streamEvents(dockerc, state)

	events := make(chan lorne.Event)
	state.AddListener("all", events)
	go syncScheduler(scheduler, events)

	var host *sampi.Host
	if *configFile != "" {
		host, err = openConfig(*configFile)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		host = &sampi.Host{Resources: make(map[string]sampi.ResourceValue)}
	}
	if _, ok := host.Resources["memory"]; !ok {
		host.Resources["memory"] = sampi.ResourceValue{Value: 1024}
	}
	host.ID = *hostID

	jobs := make(chan *sampi.Job)
	scheduler.RegisterHost(host, jobs)
	g.Log(grohl.Data{"at": "host_registered"})
	processJobs(jobs, *externalAddr, dockerc, state, ports)
}

type dockerProcessingClient interface {
	CreateContainer(*docker.Config) (*docker.Container, error)
	PullImage(docker.PullImageOptions, io.Writer) error
	StartContainer(string, *docker.HostConfig) error
}

func processJobs(jobs chan *sampi.Job, externalAddr string, dockerc dockerProcessingClient, state *State, ports <-chan int) {
	for job := range jobs {
		g := grohl.NewContext(grohl.Data{"fn": "process_job", "job.id": job.ID})
		g.Log(grohl.Data{"at": "start", "job.image": job.Config.Image, "job.cmd": job.Config.Cmd, "job.entrypoint": job.Config.Entrypoint})

		var hostConfig *docker.HostConfig
		if job.TCPPorts > 0 {
			port := strconv.Itoa(<-ports)
			job.Config.Env = append(job.Config.Env, "PORT="+port)
			job.Config.ExposedPorts = map[string]struct{}{port + "/tcp": struct{}{}}
			hostConfig = &docker.HostConfig{
				PortBindings:    map[string][]docker.PortBinding{port + "/tcp": {{HostPort: port}}},
				PublishAllPorts: true,
			}
		}
		job.Config.Name = "flynn-" + job.ID
		if externalAddr != "" {
			job.Config.Env = append(job.Config.Env, "EXTERNAL_IP="+externalAddr, "SD_HOST="+externalAddr, "DISCOVERD="+externalAddr+":1111")
		}
		state.AddJob(job)
		g.Log(grohl.Data{"at": "create_container"})
		container, err := dockerc.CreateContainer(job.Config)
		if err == docker.ErrNoSuchImage {
			g.Log(grohl.Data{"at": "pull_image"})
			err = dockerc.PullImage(docker.PullImageOptions{Repository: job.Config.Image}, os.Stdout)
			if err != nil {
				g.Log(grohl.Data{"at": "pull_image", "status": "error", "err": err})
				state.SetStatusFailed(job.ID, err)
				continue
			}
			container, err = dockerc.CreateContainer(job.Config)
		}
		if err != nil {
			g.Log(grohl.Data{"at": "create_container", "status": "error", "err": err})
			state.SetStatusFailed(job.ID, err)
			continue
		}
		state.SetContainerID(job.ID, container.ID)
		state.WaitAttach(job.ID)
		g.Log(grohl.Data{"at": "start_container"})
		if err := dockerc.StartContainer(container.ID, hostConfig); err != nil {
			g.Log(grohl.Data{"at": "start_container", "status": "error", "err": err})
			state.SetStatusFailed(job.ID, err)
			continue
		}
		state.SetStatusRunning(job.ID)
		g.Log(grohl.Data{"at": "finish"})
	}
}

type sampiSyncClient interface {
	RemoveJobs([]string) error
}

func syncScheduler(scheduler sampiSyncClient, events <-chan lorne.Event) {
	for event := range events {
		if event.Event != "stop" {
			continue
		}
		grohl.Log(grohl.Data{"fn": "scheduler_event", "at": "remove_job", "job.id": event.JobID})
		if err := scheduler.RemoveJobs([]string{event.JobID}); err != nil {
			grohl.Log(grohl.Data{"fn": "scheduler_event", "at": "remove_job", "status": "error", "err": err, "job.id": event.JobID})
			// TODO: try to reconnect?
		}
	}
}

type dockerStreamClient interface {
	Events() (*docker.EventStream, error)
	InspectContainer(string) (*docker.Container, error)
}

func streamEvents(client dockerStreamClient, state *State) {
	stream, err := client.Events()
	if err != nil {
		log.Fatal(err)
	}
	for event := range stream.Events {
		if event.Status != "die" {
			continue
		}
		container, err := client.InspectContainer(event.ID)
		if err != nil {
			log.Println("inspect container", event.ID, "error:", err)
			// TODO: set job status anyway?
			continue
		}
		state.SetStatusDone(event.ID, container.State.ExitCode)
	}
}

// TODO: fix this, horribly broken

func allocatePorts(ports chan<- int, startPort, endPort int) {
	for i := startPort; i < endPort; i++ {
		ports <- i
	}
	// TODO: handle wrap-around
}
