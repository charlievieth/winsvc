// +build windows

package winsvc

import (
	"testing"

	"golang.org/x/sys/windows/svc/mgr"
)

func BenchmarkIter(b *testing.B) {
	m, err := Connect()
	if err != nil {
		Fatal(err)
	}
	defer m.Disconnect()

	m.match = func(s string) bool { return true }

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		m.iter(func(_ *mgr.Service) error { return nil })
	}
}
