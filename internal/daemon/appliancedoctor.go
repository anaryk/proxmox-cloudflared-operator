package daemon

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// applianceDoctorCluster is what the doctor of a controller container reads
// of the cluster: its own agent, the install's CA, what the engine knows,
// and the agents of the other nodes, asked over the agent channel. A
// container reads neither the members file nor corosync.conf, and not every
// node runs an agent container: the checks that go by them find nothing.
type applianceDoctorCluster struct {
	st  *store.Store
	eng *engine.Engine
}

var _ doctor.ClusterSource = applianceDoctorCluster{}

func (d applianceDoctorCluster) Agent() (agentproto.AgentView, error) { return d.eng.AgentView() }

func (applianceDoctorCluster) Members() (members.Members, error) {
	return members.Members{}, nil
}

func (applianceDoctorCluster) Corosync() (members.Corosync, error) {
	return members.Corosync{}, nil
}

func (d applianceDoctorCluster) CAExpires() (time.Time, error) { return caExpires(d.st) }

func (d applianceDoctorCluster) Facts() engine.ClusterFacts { return d.eng.ClusterFacts() }

func (d applianceDoctorCluster) Ask(ctx context.Context, nodes []string, wait time.Duration) map[string]doctor.AgentAnswer {
	return doctorCluster{eng: d.eng}.Ask(ctx, nodes, wait)
}

// caExpires is when the current CA of the install ends; zero before there
// is one.
func caExpires(st *store.Store) (time.Time, error) {
	ca, _, _, err := st.CA()
	switch {
	case errors.Is(err, store.ErrNoCA):
		return time.Time{}, nil
	case err != nil:
		return time.Time{}, err
	}
	b, _ := pem.Decode([]byte(ca.CertPEM))
	if b == nil {
		return time.Time{}, errors.New("the CA is not a certificate in PEM")
	}
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return time.Time{}, err
	}
	return cert.NotAfter, nil
}

// agentContainerCluster is what the doctor of an agent container reads: its
// agent alone. The cluster's checks are the controller's to make.
type agentContainerCluster struct {
	view func() agentproto.AgentView
}

func (a agentContainerCluster) Agent() (agentproto.AgentView, error) { return a.view(), nil }

func (agentContainerCluster) Members() (members.Members, error) {
	return members.Members{}, nil
}

func (agentContainerCluster) Corosync() (members.Corosync, error) {
	return members.Corosync{}, nil
}

func (agentContainerCluster) CAExpires() (time.Time, error) { return time.Time{}, nil }

func (agentContainerCluster) Facts() engine.ClusterFacts { return engine.ClusterFacts{} }

func (agentContainerCluster) Ask(context.Context, []string, time.Duration) map[string]doctor.AgentAnswer {
	return map[string]doctor.AgentAnswer{}
}

// doctorHook makes the checks of an agent container for a doctor action of
// its controller.
func doctorHook(r *doctor.Runner) func(context.Context) (json.RawMessage, error) {
	return func(ctx context.Context) (json.RawMessage, error) { return json.Marshal(r.AgentChecks(ctx)) }
}
