package daemon

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// doctorCluster is what the doctor reads of the cluster on a host node: the
// agent of this node, the members and corosync files, the install's CA and
// what the engine's cycles know.
type doctorCluster struct {
	cp  *clusterPart
	eng *engine.Engine
}

var _ doctor.ClusterSource = doctorCluster{}

func (d doctorCluster) Agent() (agentproto.AgentView, error) { return d.cp.agentView() }

// Members is the members file as the lease keeper last read it.
func (d doctorCluster) Members() (members.Members, error) {
	if d.cp.quorum == nil {
		return members.Members{}, errors.New("the members of the cluster are not read on this node")
	}
	m := d.cp.quorum()
	if m.ReadAt.IsZero() {
		return members.Members{}, errors.New("the members file has not been read yet")
	}
	return m, nil
}

func (d doctorCluster) Corosync() (members.Corosync, error) { return d.cp.readCorosync() }

// CAExpires is when the current CA of the install ends; zero before setup
// made one.
func (d doctorCluster) CAExpires() (time.Time, error) {
	ca, _, _, err := d.cp.st.CA()
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

func (d doctorCluster) Facts() engine.ClusterFacts { return d.eng.ClusterFacts() }

// Ask asks no agent: this daemon does not hand out doctor actions.
func (d doctorCluster) Ask(context.Context, []string, time.Duration) map[string]doctor.AgentAnswer {
	return nil
}
