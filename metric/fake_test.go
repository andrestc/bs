// Copyright 2015 bs authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package metric

import "sync"

var fakeBackend fake

func init() {
	Register("fake", func() (Backend, error) {
		return &fakeBackend, nil
	})
}

type fakeStat struct {
	container string
	image     string
	app       string
	hostname  string
	process   string
	key       string
	value     interface{}
}

type fake struct {
	mu       sync.Mutex
	stats    []fakeStat
	failures chan error
}

type fakeStatList []fakeStat

func (s fakeStatList) Len() int           { return len(s) }
func (s fakeStatList) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s fakeStatList) Less(i, j int) bool { return s[i].container+s[i].key < s[j].container+s[j].key }

func (s *fake) Send(container ContainerInfo, key string, value interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case err := <-s.failures:
		return err
	default:
		stat := fakeStat{
			app:       container.App,
			hostname:  container.Hostname,
			process:   container.Process,
			container: container.Name,
			image:     container.Image,
			key:       key,
			value:     value,
		}
		s.stats = append(s.stats, stat)
		return nil
	}
}

func (s *fake) SendConn(container ContainerInfo, host string) error {
	return s.Send(container, "connection", host)
}

func (s *fake) SendHost(host HostInfo, key string, value interface{}) error {
	data := ContainerInfo{App: "sysapp", Process: "-", Hostname: host.Name}
	return s.Send(data, key, value)
}

func (s *fake) prepareFailure(err error) {
	s.failures <- err
}

func (s *fake) reset() {
	s.failures = make(chan error, 4)
	s.stats = nil
}
