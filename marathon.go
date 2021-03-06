package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/Sirupsen/logrus"
)

type MarathonTasks struct {
	Tasks []struct {
		AppId              string `json:"appId"`
		HealthCheckResults []struct {
			Alive bool `json:"alive"`
		} `json:"healthCheckResults"`
		Host         string  `json:"host"`
		Id           string  `json:"id"`
		Ports        []int64 `json:"ports"`
		ServicePorts []int64 `json:"servicePorts"`
		StagedAt     string  `json:"stagedAt"`
		StartedAt    string  `json:"startedAt"`
		Version      string  `json:"version"`
	} `json:"tasks"`
}

type MarathonApps struct {
	Apps []struct {
		Id           string            `json:"id"`
		Labels       map[string]string `json:"labels"`
		Env          map[string]string `json:"env"`
		HealthChecks []interface{}     `json:"healthChecks"`
	} `json:"apps"`
}

func eventStream() {
	go func() {
		client := &http.Client{
			Timeout:   0 * time.Second,
			Transport: tr,
		}
		ticker := time.NewTicker(1 * time.Second)
		for _ = range ticker.C {
			req, err := http.NewRequest("GET", endpoint+"/v2/events", nil)
			if err != nil {
				logger.WithFields(logrus.Fields{
					"error":    err.Error(),
					"endpoint": endpoint,
				}).Error("unable to create event stream request")
				continue
			}
			req.Header.Set("Accept", "text/event-stream")
			if config.User != "" {
				req.SetBasicAuth(config.User, config.Pass)
			}
			resp, err := client.Do(req)
			if err != nil {
				logger.WithFields(logrus.Fields{
					"error":    err.Error(),
					"endpoint": endpoint,
				}).Error("unable to access Marathon event stream")
				continue
			}
			reader := bufio.NewReader(resp.Body)
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					if err != io.EOF {
						logger.WithFields(logrus.Fields{
							"error":    err.Error(),
							"endpoint": endpoint,
						}).Error("error reading Marathon event stream")
					}
					resp.Body.Close()
					break
				}
				if !strings.HasPrefix(line, "event: ") {
					continue
				}
				logger.WithFields(logrus.Fields{
					"event":    strings.TrimSpace(line[6:]),
					"endpoint": endpoint,
				}).Info("marathon event received")
				select {
				case eventqueue <- true: // Add reload to our queue channel, unless it is full of course.
				default:
					logger.Warn("queue is full")
				}
			}
			resp.Body.Close()
			logger.Warn("event stream connection was closed, re-opening")
		}
	}()
}

func endpointHealth() {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		for {
			select {
			case <-ticker.C:
				tmpsick := 0
				for _, ep := range config.Marathon {
					client := &http.Client{
						Timeout:   5 * time.Second,
						Transport: tr,
					}
					req, err := http.NewRequest("GET", ep+"/ping", nil)
					if err != nil {
						logger.WithFields(logrus.Fields{
							"error":    err.Error(),
							"endpoint": ep,
						}).Error("an error occurred creating endpoint health request")
						tmpsick += 1
						continue
					}
					if config.User != "" {
						req.SetBasicAuth(config.User, config.Pass)
					}
					resp, err := client.Do(req)
					if err != nil {
						logger.WithFields(logrus.Fields{
							"error":    err.Error(),
							"endpoint": ep,
						}).Error("endpoint is down")
						tmpsick += 1
						continue
					}
					resp.Body.Close()
					if resp.StatusCode != 200 {
						logger.WithFields(logrus.Fields{
							"status":   resp.StatusCode,
							"endpoint": ep,
						}).Error("endpoint check failed")
						tmpsick += 1
						continue
					}
					if endpoint != ep {
						endpoint = ep
						logger.WithFields(logrus.Fields{
							"endpoint": ep,
						}).Info("new endpoint is active")
					}
					break // no need to continue now.
				}
				sick = tmpsick
			}
		}
	}()
}

func eventWorker() {
	go func() {
		// a ticker channel to limit reloads to marathon, 1s is enough for now.
		ticker := time.NewTicker(1 * time.Second)
		for {
			select {
			case <-ticker.C:
				<-eventqueue
				start := time.Now()
				err := reload()
				elapsed := time.Since(start)
				if err != nil {
					logger.Error("config update failed")
					go statsCount("reload.failed", 1)
				} else {
					logger.WithFields(logrus.Fields{
						"took": elapsed,
					}).Info("config updated")
					go statsCount("reload.success", 1)
					go statsTiming("reload.time", elapsed)
				}
			}
		}
	}()
}

func fetchApps(jsontasks *MarathonTasks, jsonapps *MarathonApps) error {
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: tr,
	}
	// take advantage of goroutines and run both reqs concurrent.
	appschn := make(chan error)
	taskschn := make(chan error)
	go func() {
		req, err := http.NewRequest("GET", endpoint+"/v2/tasks", nil)
		if err != nil {
			taskschn <- err
			return
		}
		req.Header.Set("Accept", "application/json")
		if config.User != "" {
			req.SetBasicAuth(config.User, config.Pass)
		}
		resp, err := client.Do(req)
		if err != nil {
			taskschn <- err
			return
		}
		defer resp.Body.Close()
		decoder := json.NewDecoder(resp.Body)
		err = decoder.Decode(&jsontasks)
		if err != nil {
			taskschn <- err
			return
		}
		taskschn <- nil
	}()
	go func() {
		req, err := http.NewRequest("GET", endpoint+"/v2/apps", nil)
		if err != nil {
			appschn <- err
			return
		}
		req.Header.Set("Accept", "application/json")
		if config.User != "" {
			req.SetBasicAuth(config.User, config.Pass)
		}
		resp, err := client.Do(req)
		if err != nil {
			appschn <- err
			return
		}
		defer resp.Body.Close()
		decoder := json.NewDecoder(resp.Body)
		err = decoder.Decode(&jsonapps)
		if err != nil {
			appschn <- err
			return
		}
		appschn <- nil
	}()
	appserr := <-appschn
	taskserr := <-taskschn
	if appserr != nil {
		return appserr
	}
	if taskserr != nil {
		return taskserr
	}
	return nil
}

func syncApps(jsontasks *MarathonTasks, jsonapps *MarathonApps) {
	config.Lock()
	defer config.Unlock()
	config.Apps = make(map[string]App)
	for _, app := range jsonapps.Apps {
	OUTER:
		for _, task := range jsontasks.Tasks {
			if task.AppId != app.Id {
				continue
			}
			// Lets skip tasks that does not expose any ports.
			if len(task.Ports) == 0 {
				continue
			}
			if len(app.HealthChecks) > 0 {
				if len(task.HealthCheckResults) == 0 {
					// this means tasks is being deployed but not yet monitored as alive. Assume down.
					continue
				}
				alive := true
				for _, health := range task.HealthCheckResults {
					// check if health check is alive
					if health.Alive == false {
						alive = false
					}
				}
				if alive != true {
					// at least one health check has failed. Assume down.
					continue
				}
			}
			if s, ok := config.Apps[app.Id]; ok {
				s.Tasks = append(s.Tasks, task.Host+":"+strconv.FormatInt(task.Ports[0], 10))
				config.Apps[app.Id] = s
			} else {
				var newapp = App{}
				newapp.Tasks = []string{task.Host + ":" + strconv.FormatInt(task.Ports[0], 10)}
				if s, ok := app.Labels["subdomain"]; ok {
					hosts := strings.Split(s, " ")
					for _, host := range hosts {
						newapp.Hosts = append(newapp.Hosts, host)
					}
				} else if s, ok := app.Labels["moxy_subdomain"]; ok {
					// to be compatible with moxy
					hosts := strings.Split(s, " ")
					for _, host := range hosts {
						newapp.Hosts = append(newapp.Hosts, host)
					}
				} else {
					// return the base name of app id, needed if directories are used.
					base := filepath.Base(app.Id)
					newapp.Hosts = append(newapp.Hosts, base)
				}
				for _, confapp := range config.Apps {
					for _, host := range confapp.Hosts {
						for _, newhost := range newapp.Hosts {
							if newhost == host {
								logger.WithFields(logrus.Fields{
									"app":       app.Id,
									"subdomain": host,
								}).Warn("duplicate subdomain label, ignoring app")
								continue OUTER
							}
						}
					}
				}
				newapp.Labels = app.Labels
				newapp.Env = app.Env
				config.Apps[app.Id] = newapp
			}
		}

	}

}

func writeConf() error {
	t, err := template.New(filepath.Base(config.Nginx_template)).ParseFiles(config.Nginx_template)
	if err != nil {
		return err
	}
	f, err := os.Create(config.Nginx_config)
	defer f.Close()
	if err != nil {
		return err
	}
	err = t.Execute(f, config)
	if err != nil {
		return err
	}
	return nil
}

func checkTmpl() error {
	t, err := template.New(filepath.Base(config.Nginx_template)).ParseFiles(config.Nginx_template)
	if err != nil {
		return err
	}
	err = t.Execute(ioutil.Discard, config)
	if err != nil {
		return err
	}
	return nil
}

func checkConf() error {
	cmd := exec.Command(config.Nginx_cmd, "-c", config.Nginx_config, "-t")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run() // will wait for command to return
	if err != nil {
		msg := fmt.Sprint(err) + ": " + stderr.String()
		errstd := errors.New(msg)
		return errstd
	}
	return nil
}

func reloadNginx() error {
	cmd := exec.Command(config.Nginx_cmd, "-s", "reload")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run() // will wait for command to return
	if err != nil {
		msg := fmt.Sprint(err) + ": " + stderr.String()
		errstd := errors.New(msg)
		return errstd
	}
	return nil
}

func reload() error {
	jsontasks := MarathonTasks{}
	jsonapps := MarathonApps{}
	err := fetchApps(&jsontasks, &jsonapps)
	if err != nil {
		logger.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Error("unable to sync from marathon")
		return err
	}
	syncApps(&jsontasks, &jsonapps)
	err = writeConf()
	if err != nil {
		logger.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Error("unable to generate nginx config")
		return err
	}
	err = reloadNginx()
	if err != nil {
		logger.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Error("unable to reload nginx")
		return err
	}
	return nil
}
