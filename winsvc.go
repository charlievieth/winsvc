// +build windows

package winsvc

import (
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

func isAccessDenied(err error) bool { return err == syscall.ERROR_ACCESS_DENIED }

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

// https://msdn.microsoft.com/en-us/library/windows/desktop/ms681383(v=vs.85).aspx
const ERROR_SERVICE_ALREADY_RUNNING = syscall.Errno(0x420)

func (m *Mgr) Start() (first error) {
	return m.iter(func(s *mgr.Service) error {
		if err := m.setStartType(s, mgr.StartManual); err != nil {
			fmt.Printf("START ERROR - StartType: Service (%s): %s\n", s.Name, err)
			return err
		}
		if err := s.Start(); err != nil {
			// Ignore error if the service is running
			if err != ERROR_SERVICE_ALREADY_RUNNING {
				fmt.Printf("START ERROR - Start: Service (%s): %s\n", s.Name, err)
				return err
			}
		}
		return nil
	})
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

const (
	ERROR_SERVICE_CANNOT_ACCEPT_CTRL = syscall.Errno(0X425)
	ERROR_SERVICE_NOT_ACTIVE         = syscall.Errno(0X426)
)

func (m *Mgr) stopPending(s *mgr.Service) error {
	const Timeout = 15 * time.Second
	const Interval = 100 * time.Millisecond

	// Loop while the service state is StartPending
	//
	var status svc.Status
	start := time.Now()
	for {
		var err error
		status, err = s.Query()
		if err != nil {
			return fmt.Errorf("winsvc: querying status of service (%s): %s",
				s.Name, err)
		}
		if status.State != svc.StartPending {
			break
		}
		if time.Since(start) > Timeout {
			return fmt.Errorf("winsvc: timed waiting for service (%s) to start",
				s.Name)
		}
		time.Sleep(Interval)
	}

	switch status.State {
	case svc.Stopped, svc.StopPending:
		// Ok, but how is this possible?
	case svc.StartPending:
		// This should not happen
		return fmt.Errorf("winsvc: invalid state for service: %s", s.Name)
	default:
		_, err := s.Control(svc.Stop)
		return err
	}

	return nil
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

func (m *Mgr) stopService(s *mgr.Service) error {
	// Set start type to disabled
	if err := m.setStartType(s, mgr.StartDisabled); err != nil {
		return err
	}

	// Send stop command
	if st, err := s.Control(svc.Stop); err != nil {
		switch err {
		case ERROR_SERVICE_CANNOT_ACCEPT_CTRL:
			switch st.State {
			case svc.Stopped, svc.StopPending:
				// Ok
			case svc.StartPending:
				if err := m.stopPending(s); err != nil {
					return err
				}
			default:
				return fmt.Errorf("winsvc: service (%s) invalid state (%s) for error: %s",
					s.Name, svcStateString(st.State), err)
			}
		case ERROR_SERVICE_NOT_ACTIVE:
			// Ignore the service is not running
		default:
			return fmt.Errorf("winsvc: stopping service (%s): %s", s.Name, err)
		}
	}
	return nil
}

func (m *Mgr) Stop() (first error) {
	return m.iter(func(s *mgr.Service) error {
		return m.stopService(s)
	})
}

func (m *Mgr) Delete() (first error) {
	svcs, err := m.services()
	if err != nil {
		return err
	}
	for _, s := range svcs {
		if err := s.Delete(); err != nil {
			if first == nil {
				first = err
			}
		}
	}
	running := true
	for running {
		for _, s := range svcs {
			if s == nil {
				continue
			}
			st, err := s.Query()
			if err != nil {
				if first == nil {
					first = err
				}
				s.Close()
				s = nil
				continue
			}
			running = running && st.State == svc.Stopped
			if st.State == svc.Stopped {
				s.Close()
				s = nil
			}
		}
	}
	return
}

var windowsSvcStateStr = [...]string{
	windows.SERVICE_STOPPED:          "stopped",          // 1
	windows.SERVICE_START_PENDING:    "starting",         // 2
	windows.SERVICE_STOP_PENDING:     "stop_pending",     // 3
	windows.SERVICE_RUNNING:          "running",          // 4
	windows.SERVICE_CONTINUE_PENDING: "continue_pending", // 5
	windows.SERVICE_PAUSE_PENDING:    "pause_pending",    // 6
	windows.SERVICE_PAUSED:           "paused",           // 7
}

func (m *Mgr) State() ([]string, error) {
	var states []string
	err := m.iter(func(s *mgr.Service) error {
		q, err := s.Query()
		if err != nil {
			return err
		}
		i := int(q.State)
		if 0 < i && i < len(windowsSvcStateStr) {
			states = append(states, windowsSvcStateStr[i])
		} else {
			states = append(states, "Invalid Service State")
		}
		return nil
	})
	return states, err
}

/*
func main() {
	m, err := Connect()
	if err != nil {
		Fatal(err)
	}
	defer m.Disconnect()

	names, err := m.m.ListServices()
	if err != nil {
		Fatal(err)
	}
	for _, name := range names {
		s, err := m.m.OpenService(name)
		if err != nil {
			fmt.Println("Open:", name)
			continue
		}
		c, err := s.Config()
		s.Close()
		if err != nil {
			fmt.Println("Conf:", name)
			continue
		}
		_ = c
	}

	// m.match = func(s string) bool {
	// 	// s = strings.ToLower(s)
	// 	return strings.Contains(s, "Window")
	// 	// return strings.Contains(s, "window") || strings.Contains(s, "microsoft")
	// }
	// start := time.Now()
	// first := m.iter(func(s *mgr.Service) error {
	// 	fmt.Println(s.Name)
	// 	return nil
	// })
	// fmt.Println(time.Since(start))
	// fmt.Println(first)
}
*/

/*
func Fatal(err interface{}) {
	if err == nil {
		return
	}
	_, file, line, ok := runtime.Caller(1)
	if ok {
		file = filepath.Base(file)
	}
	switch err.(type) {
	case error, string, fmt.Stringer:
		if ok {
			fmt.Fprintf(os.Stderr, "Error (%s:%d): %s", file, line, err)
		} else {
			fmt.Fprintf(os.Stderr, "Error: %s", err)
		}
	default:
		if ok {
			fmt.Fprintf(os.Stderr, "Error (%s:%d): %#v\n", file, line, err)
		} else {
			fmt.Fprintf(os.Stderr, "Error: %#v\n", err)
		}
	}
	os.Exit(1)
}
*/
