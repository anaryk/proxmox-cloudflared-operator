package daemon

import (
	sd "github.com/coreos/go-systemd/v22/daemon"
)

// Notifier tells the service manager how the daemon is doing.
type Notifier interface {
	// Ready says that the daemon serves its socket.
	Ready() error
	// Stopping says that the daemon begins to stop.
	Stopping() error
}

// systemdNotifier speaks sd_notify. Without a notify socket, which is the case
// outside a unit of Type=notify, both calls do nothing.
type systemdNotifier struct{}

func (systemdNotifier) Ready() error {
	_, err := sd.SdNotify(false, sd.SdNotifyReady)
	return err
}

func (systemdNotifier) Stopping() error {
	_, err := sd.SdNotify(false, sd.SdNotifyStopping)
	return err
}
