package pve

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

func TestHAResources(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{"/cluster/ha/resources": okReply(t, "ha_resources.json")})
	got, err := c.HAResources(context.Background())
	require.NoError(t, err)
	require.Equal(t, []HAResource{
		{SID: "ct:9451", Ref: model.GuestRef{Kind: model.KindLXC, VMID: 9451}, State: "started"},
		{SID: "vm:101", Ref: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, State: "disabled", Group: "prod"},
	}, got)
	require.Equal(t, "/api2/json/cluster/ha/resources", rec.requests()[0].uri)
}

func TestHAResourcesEmpty(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{"/cluster/ha/resources": {http.StatusOK, `{"data":[]}`}})
	got, err := c.HAResources(context.Background())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestHAResourcesRejectsAServiceThatIsNoGuest(t *testing.T) {
	for _, sid := range []string{"", "ct", "ct:", "ct:0", "ct:-1", "ct:+5", "ct: 5", "ct:05", "ct:5a", "ct:abc", "vm:101:2", "node:pve1", "9451"} {
		t.Run(sid, func(t *testing.T) {
			c, _ := newTestClient(t, map[string]reply{
				"/cluster/ha/resources": {http.StatusOK, `{"data":[{"sid":"ct:9451","state":"started"},{"sid":"` + sid + `","state":"started"}]}`},
			})
			got, err := c.HAResources(context.Background())
			require.Error(t, err)
			require.Nil(t, got)
		})
	}
}

func TestHAStatusReadsMaintenanceFromTheReportOfTheNode(t *testing.T) {
	tests := []struct {
		file string
		want HAStatus
	}{
		{
			file: "ha_status_maintenance.json",
			want: HAStatus{
				Maintenance: map[string]Maintenance{"pco-lab-1": MaintenanceOff, "pco-lab-2": MaintenanceOff, "pco-lab-3": MaintenanceOn},
				Services:    map[string]HAService{"ct:9451": {Node: "pco-lab-1", State: "started"}},
			},
		},
		{
			file: "ha_status_idle.json",
			want: HAStatus{
				Maintenance: map[string]Maintenance{"pco-lab-1": MaintenanceOff, "pco-lab-2": MaintenanceOff, "pco-lab-3": MaintenanceOff},
				Services:    map[string]HAService{"ct:9451": {Node: "pco-lab-1", State: "started"}},
			},
		},
		{
			// A node without HA resources has no resource manager to report.
			file: "ha_status_standalone.json",
			want: HAStatus{Maintenance: map[string]Maintenance{}, Services: map[string]HAService{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			c, rec := newTestClient(t, map[string]reply{"/cluster/ha/status/current": okReply(t, tt.file)})
			got, err := c.HAStatus(context.Background())
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			require.Equal(t, "/api2/json/cluster/ha/status/current", rec.requests()[0].uri)
		})
	}
}

// The mode of a resource manager is only in the text of its report, before
// the first comma: a state, or a mode followed by the word mode. A report
// that is stale or could not be read says nothing of the mode.
func TestMaintenanceIsReadFromTheTextOfTheReport(t *testing.T) {
	tests := []struct {
		status string
		want   Maintenance
	}{
		{"pco-lab-3 (maintenance mode, watchdog standby, Tue Oct 13 22:51:35 2026)", MaintenanceOn},
		{"pco-lab-3 (maintenance mode)", MaintenanceOn},
		{"pco-lab-3 ( Maintenance Mode , watchdog standby)", MaintenanceOn},
		{"pco-lab-3 (active, watchdog active, Tue Oct 13 22:51:30 2026)", MaintenanceOff},
		{"pco-lab-3 (idle, watchdog standby, Tue Oct 13 22:52:05 2026)", MaintenanceOff},
		{"pco-lab-3 (wait_for_agent_lock, watchdog standby, Tue Oct 13 22:52:05 2026)", MaintenanceOff},
		{"pco-lab-3 (lost_agent_lock, watchdog active, Tue Oct 13 22:52:05 2026)", MaintenanceOff},
		{"pco-lab-3 (restart mode, watchdog active, Tue Oct 13 22:52:05 2026)", MaintenanceOff},
		{"pco-lab-3 (shutdown mode, watchdog active, Tue Oct 13 22:52:05 2026)", MaintenanceOff},
		{"pco-lab-3 (old timestamp - dead?, watchdog standby, Tue Oct 13 22:52:05 2026)", MaintenanceUnknown},
		{"pco-lab-3 (unable to read lrm status)", MaintenanceUnknown},
		{"pco-lab-3 (unknown)", MaintenanceUnknown},
		{"pco-lab-3 ()", MaintenanceUnknown},
		{"maintenance mode", MaintenanceUnknown},
		{"pco-lab-3", MaintenanceUnknown},
		{"", MaintenanceUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			require.Equal(t, tt.want, maintenanceOf(tt.status))
		})
	}
	require.Equal(t, "unknown", MaintenanceUnknown.String())
	require.Equal(t, "off", MaintenanceOff.String())
	require.Equal(t, "on", MaintenanceOn.String())
}

func TestHAStatusTakesTheNodeAndTheServiceFromTheIDWhenTheFieldsAreMissing(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{"/cluster/ha/status/current": {http.StatusOK, `{"data":[
		{"id":"lrm:pco-lab-3","status":"pco-lab-3 (maintenance mode, watchdog standby, Tue Oct 13 22:51:35 2026)","type":"lrm"},
		{"id":"service:vm:101","node":"pco-lab-2","state":"started","type":"service"},
		{"id":"something:new","node":"pco-lab-2","type":"later"}
	]}`}})
	got, err := c.HAStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, HAStatus{
		Maintenance: map[string]Maintenance{"pco-lab-3": MaintenanceOn},
		Services:    map[string]HAService{"vm:101": {Node: "pco-lab-2", State: "started"}},
	}, got)
}

func TestHAStatusRejectsAReportWithoutANodeOrAService(t *testing.T) {
	for name, row := range map[string]string{
		"a resource manager without a node": `{"id":"lrm:","status":"(idle)","type":"lrm"}`,
		"a service without an id":           `{"id":"service:","node":"pco-lab-1","state":"started","type":"service"}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := newTestClient(t, map[string]reply{"/cluster/ha/status/current": {http.StatusOK, `{"data":[` + row + `]}`}})
			_, err := c.HAStatus(context.Background())
			require.Error(t, err)
		})
	}
}

func TestHAReadsPassTheErrorOfTheAPIOn(t *testing.T) {
	c := newErrorClient(t, http.StatusForbidden, `{"data":null,"message":"Permission check failed (/, Sys.Audit)"}`)
	_, err := c.HAStatus(context.Background())
	require.True(t, IsForbidden(err))
	_, err = c.HAResources(context.Background())
	require.True(t, IsForbidden(err))
}
