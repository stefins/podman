package notifyproxy

import (
	"testing"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/stretchr/testify/require"
)

// Helper function to send the specified message over the socket of the proxy.
func sendMessage(t *testing.T, proxy *NotifyProxy, message string) {
	err := SendMessage(proxy.SocketPath(), message)
	require.NoError(t, err)
}

func TestNotifyProxy(t *testing.T) {
	proxy, err := New("")
	require.NoError(t, err)
	require.FileExists(t, proxy.SocketPath())
	require.NoError(t, proxy.close())
	require.NoFileExists(t, proxy.SocketPath())
}

func TestWaitAndClose(t *testing.T) {
	proxy, err := New("")
	require.NoError(t, err)
	require.FileExists(t, proxy.SocketPath())

	ch := make(chan error)

	go func() {
		ch <- proxy.WaitAndClose()
	}()

	sendMessage(t, proxy, "foo\n")
	time.Sleep(250 * time.Millisecond)
	select {
	case err := <-ch:
		t.Fatalf("Should still be waiting but received %v", err)
	default:
	}

	sendMessage(t, proxy, daemon.SdNotifyReady+"\nsomething else")
	done := func() bool {
		for i := 0; i < 10; i++ {
			select {
			case err := <-ch:
				require.NoError(t, err, "Waiting should succeed")
				return true
			default:
				time.Sleep(time.Duration(i*250) * time.Millisecond)
			}
		}
		return false
	}()
	require.True(t, done, "READY MESSAGE SHOULD HAVE ARRIVED")
}
