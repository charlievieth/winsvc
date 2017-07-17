// +build windows

package winsvc

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const ServiceDescription = "vcap"

type Mgr struct {
	m     *mgr.Mgr
	match func(description string) bool // WARN: DEV ONLY
}

func Connect(match func(description string) bool) (*Mgr, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, err
	}
	if match == nil {
		match = func(_ string) bool { return true }
	}
	return &Mgr{m: m, match: match}, nil
}

func (m *Mgr) Disconnect() error {
	return m.m.Disconnect()
}

func toString(p *uint16) string {
	if p == nil {
		return ""
	}
	return syscall.UTF16ToString((*[4096]uint16)(unsafe.Pointer(p))[:])
}

func (m *Mgr) serviceDescription(s *mgr.Service) (string, error) {
	var p *windows.SERVICE_DESCRIPTION
	n := uint32(1024)
	for {
		b := make([]byte, n)
		p = (*windows.SERVICE_DESCRIPTION)(unsafe.Pointer(&b[0]))
		err := windows.QueryServiceConfig2(s.Handle,
			windows.SERVICE_CONFIG_DESCRIPTION, &b[0], n, &n)
		if err == nil {
			break
		}
		if err.(syscall.Errno) != syscall.ERROR_INSUFFICIENT_BUFFER {
			return "", err
		}
		if n <= uint32(len(b)) {
			return "", err
		}
	}
	return toString(p.Description), nil
}

func (m *Mgr) services() ([]*mgr.Service, error) {
	names, err := m.m.ListServices()
	if err != nil {
		return nil, err
	}
	var svcs []*mgr.Service
	for _, name := range names {
		s, err := m.m.OpenService(name)
		if err != nil {
			continue
		}
		desc, err := m.serviceDescription(s)
		if err != nil {
			s.Close()
			continue
		}
		if m.match(desc) {
			svcs = append(svcs, s)
		} else {
			s.Close()
		}
	}
	return svcs, nil
}

func (m *Mgr) iter(fn func(*mgr.Service) error) (first error) {
	svcs, err := m.services()
	if err != nil {
		return err
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range svcs {
		wg.Add(1)
		go func(s *mgr.Service) {
			defer wg.Done()
			defer s.Close()
			if err := fn(s); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}(s)
	}
	wg.Wait()
	return
}

func (m *Mgr) setStartType(s *mgr.Service, startType uint32) error {
	conf, err := s.Config()
	if err != nil {
		return fmt.Errorf("winsvc: querying config for service (%s): %s",
			s.Name, err)
	}
	if conf.StartType == startType {
		return nil
	}
	conf.StartType = startType
	if err := s.UpdateConfig(conf); err != nil {
		return fmt.Errorf("winsvc: setting start type of service (%s) to (%s): %s",
			s.Name, svcStartTypeString(startType), err)
	}
	return nil
}

// https://msdn.microsoft.com/en-us/library/windows/desktop/ms681383(v=vs.85).aspx
const ERROR_SERVICE_ALREADY_RUNNING = syscall.Errno(0x420)

func (m *Mgr) doStart(s *mgr.Service) error {
	if err := m.setStartType(s, mgr.StartManual); err != nil {
		return err
	}

	status, err := s.Query()
	if err != nil {
		return fmt.Errorf("winsvc: querying status of service (%s): %s", s.Name, err)
	}

	// Check if the service is already running
	if status.State != svc.Stopped && status.State != svc.StopPending {
		return nil
	}

	start := time.Now()
	oldCheckpoint := status.CheckPoint

	for status.State == svc.StopPending {
		// Do not wait longer than the wait hint. A good interval is
		// one-tenth of the wait hint but not less than 1 second
		// and not more than 10 seconds.
		//
		hint := time.Duration(status.WaitHint) * time.Millisecond
		if hint < time.Second*10 {
			hint = time.Second * 10
		}

		wait := hint / 10
		switch {
		case wait < time.Second:
			wait = time.Second
		case wait > time.Second*10:
			wait = time.Second * 10
		}
		time.Sleep(wait)

		status, err = s.Query()
		if err != nil {
			return fmt.Errorf("winsvc: querying status of service (%s): %s",
				s.Name, err)
		}

		switch {
		case status.CheckPoint > oldCheckpoint:
			start = time.Now()
			oldCheckpoint = status.CheckPoint
		case time.Since(start) > hint:
			return fmt.Errorf("winsvc: start service: timeout waiting for service to stop: %s", s.Name)
		}
	}

	if err := s.Start(); err != nil {
		// Ignore error if the service is running
		if err != ERROR_SERVICE_ALREADY_RUNNING {
			return fmt.Errorf("winsvc: starting service (%s): %s", s.Name, err)
		}
	}

	status, err = s.Query()
	if err != nil {
		return fmt.Errorf("winsvc: querying status of service (%s): %s", s.Name, err)
	}

	start = time.Now()
	oldCheckpoint = status.CheckPoint

	for status.State == svc.StartPending {

		hint := time.Duration(status.WaitHint) * time.Millisecond
		if hint < time.Second*10 {
			hint = time.Second * 10
		}

		wait := hint / 10
		switch {
		case wait < time.Second:
			wait = time.Second
		case wait > time.Second*10:
			wait = time.Second * 10
		}
		time.Sleep(wait)

		status, err = s.Query()
		if err != nil {
			return fmt.Errorf("winsvc: querying status of service (%s): %s",
				s.Name, err)
		}

		switch {
		case status.CheckPoint > oldCheckpoint:
			start = time.Now()
			oldCheckpoint = status.CheckPoint
		case time.Since(start) > hint:
			// No progress made within the wait hint.
			break
		}
	}

	if status.State != svc.Running {
		return fmt.Errorf("winsvc: start service: service not started. "+
			"Name: %s State: %s Checkpoint: %d WaitHint: %d", s.Name,
			svcStateString(status.State), status.CheckPoint, status.WaitHint)
	}
	return nil
}

func (m *Mgr) Start() (first error) {
	return m.iter(m.doStart)
}

func (m *Mgr) doStop(s *mgr.Service) error {
	const Timeout = time.Second * 30

	if err := m.setStartType(s, mgr.StartDisabled); err != nil {
		return err
	}

	status, err := s.Query()
	if err != nil {
		return fmt.Errorf("winsvc: querying status of service (%s): %s", s.Name, err)
	}

	// Check if the service is already stopped
	if status.State == svc.Stopped {
		return nil
	}

	// If a stop is pending, wait for it

	start := time.Now()
	for status.State == svc.StopPending {
		// Do not wait longer than the wait hint. A good interval is
		// one-tenth of the wait hint but not less than 1 second
		// and not more than 10 seconds.
		//
		wait := time.Duration(status.WaitHint) * time.Millisecond / 10
		switch {
		case wait < time.Second:
			wait = time.Second
		case wait > time.Second*10:
			wait = time.Second * 10
		}
		time.Sleep(wait)

		status, err = s.Query()
		if err != nil {
			return fmt.Errorf("winsvc: querying status of service (%s): %s",
				s.Name, err)
		}

		if status.State == svc.Stopped {
			return nil // Exit
		}
		if time.Since(start) > Timeout {
			return fmt.Errorf("winsvc: stop service: timeout waiting for service to stop: %s",
				s.Name)
		}
	}

	// If a start is pending, wait for it

	start = time.Now()
	oldCheckpoint := status.CheckPoint
	for status.State == svc.StartPending {

		hint := time.Duration(status.WaitHint) * time.Millisecond
		if hint < time.Second*10 {
			hint = time.Second * 10
		}
		wait := hint / 10
		switch {
		case wait < time.Second:
			wait = time.Second
		case wait > time.Second*10:
			wait = time.Second * 10
		}
		time.Sleep(wait)

		status, err = s.Query()
		if err != nil {
			return fmt.Errorf("winsvc: querying status of service (%s): %s",
				s.Name, err)
		}

		switch {
		case status.CheckPoint > oldCheckpoint:
			start = time.Now()
			oldCheckpoint = status.CheckPoint
		case time.Since(start) > hint:
			// No progress made within the wait hint.
			break
		}
	}

	// Stop service

	status, err = s.Control(svc.Stop)
	if err != nil {
		return fmt.Errorf("winsvc: stopping service (%s): %s", s.Name, err)
	}

	var emptyStatus svc.Status
	if status == emptyStatus {
		status, err = s.Query()
		if err != nil {
			return fmt.Errorf("winsvc: querying status of service (%s): %s",
				s.Name, err)
		}
	}

	start = time.Now()
	for status.State != svc.Stopped {
		hint := time.Duration(status.WaitHint) * time.Millisecond
		if hint < time.Second*10 {
			hint = time.Second * 10
		}
		wait := hint / 10
		switch {
		case wait < time.Second:
			wait = time.Second
		case wait > time.Second*10:
			wait = time.Second * 10
		}
		time.Sleep(wait)

		status, err = s.Query()
		if err != nil {
			return fmt.Errorf("winsvc: querying status of service (%s): %s",
				s.Name, err)
		}

		if status.State == svc.Stopped {
			break
		}
		if time.Since(start) > Timeout {
			return fmt.Errorf("winsvc: stop service: timeout waiting for service to stop: %s",
				s.Name)
		}
	}

	return nil
}

func (m *Mgr) Stop() (first error) {
	return m.iter(m.doStop)
}

func (m *Mgr) Delete() error {
	const Timeout = time.Second * 30

	err := m.iter(func(s *mgr.Service) error {
		return s.Delete()
	})
	if err != nil {
		return err
	}
	start := time.Now()
	for {
		svcs, err := m.services()
		if err != nil {
			return err
		}
		if len(svcs) == 0 {
			break
		}
		for _, s := range svcs {
			s.Close()
		}
		if time.Since(start) > Timeout {
			return errors.New("winsvc: timeout waiting for services to be deleted")
		}
		time.Sleep(time.Millisecond * 500)
	}
	return nil
}

// var windowsSvcStateStr = [...]string{
// 	windows.SERVICE_STOPPED:          "stopped",          // 1
// 	windows.SERVICE_START_PENDING:    "starting",         // 2
// 	windows.SERVICE_STOP_PENDING:     "stop_pending",     // 3
// 	windows.SERVICE_RUNNING:          "running",          // 4
// 	windows.SERVICE_CONTINUE_PENDING: "continue_pending", // 5
// 	windows.SERVICE_PAUSE_PENDING:    "pause_pending",    // 6
// 	windows.SERVICE_PAUSED:           "paused",           // 7
// }

type ServiceStatus struct {
	Name  string
	State svc.State
}

func (s *ServiceStatus) StateString() string {
	return svcStateString(s.State)
}

func (m *Mgr) Status() ([]ServiceStatus, error) {
	svcs, err := m.services()
	if err != nil {
		return nil, err
	}
	defer closeServices(svcs)

	sts := make([]ServiceStatus, len(svcs))
	for i, s := range svcs {
		status, err := s.Query()
		if err != nil {
			return nil, err
		}
		sts[i] = ServiceStatus{Name: s.Name, State: status.State}
	}
	return sts, nil
}

func (m *Mgr) Unmonitor() error {
	return m.iter(func(s *mgr.Service) error {
		return m.setStartType(s, mgr.StartDisabled)
	})
}

func (m *Mgr) DisableAgentAutoStart() error {
	const name = "bosh-agent"
	s, err := m.m.OpenService("bosh-agent")
	if err != nil {
		return fmt.Errorf("winsvc: opening service (%s): %s", name, err)
	}
	defer s.Close()
	return m.setStartType(s, mgr.StartDisabled)
}

func closeServices(svcs []*mgr.Service) (first error) {
	for _, s := range svcs {
		if s == nil || s.Handle == windows.InvalidHandle {
			continue
		}
		if err := s.Close(); err != nil && first == nil {
			first = err
		}
		s.Handle = windows.InvalidHandle
	}
	return
}

func svcStateString(s svc.State) string {
	switch s {
	case svc.Stopped:
		return "Stopped"
	case svc.StartPending:
		return "StartPending"
	case svc.StopPending:
		return "StopPending"
	case svc.Running:
		return "Running"
	case svc.ContinuePending:
		return "ContinuePending"
	case svc.PausePending:
		return "PausePending"
	case svc.Paused:
		return "Paused"
	}
	return fmt.Sprintf("Invalid Service State: %d", s)
}

func svcStartTypeString(startType uint32) string {
	switch startType {
	case mgr.StartManual:
		return "StartManual"
	case mgr.StartAutomatic:
		return "StartAutomatic"
	case mgr.StartDisabled:
		return "StartDisabled"
	}
	return fmt.Sprintf("Invalid Service StartType: %d", startType)
}
