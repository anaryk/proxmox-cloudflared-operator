package daemon

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
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
	px  *pve.Client
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

// ApplianceFacts reads where the controller's container and the agent
// containers of the registry are, as the Proxmox API says.
func (d applianceDoctorCluster) ApplianceFacts(ctx context.Context) (doctor.ApplianceFacts, error) {
	inst, found, err := d.st.Install()
	switch {
	case err != nil:
		return doctor.ApplianceFacts{}, err
	case !found || inst.Appliance == nil:
		return doctor.ApplianceFacts{}, errors.New("the install records no appliance")
	}
	res, err := d.px.Resources(ctx)
	if err != nil {
		return doctor.ApplianceFacts{}, err
	}
	ha, err := d.px.HAResources(ctx)
	if err != nil {
		return doctor.ApplianceFacts{}, err
	}
	jobs, err := d.px.Replication(ctx)
	if err != nil {
		return doctor.ApplianceFacts{}, err
	}
	isHA := func(vmid int) bool {
		return slices.ContainsFunc(ha, func(r pve.HAResource) bool { return r.Ref == model.GuestRef{Kind: model.KindLXC, VMID: vmid} })
	}
	f := doctor.ApplianceFacts{VMID: inst.Appliance.VMID, HA: isHA(inst.Appliance.VMID), Storage: inst.Appliance.Storage}
	for _, j := range jobs {
		if j.Guest.VMID == f.VMID {
			f.Replication = append(f.Replication, j.ID)
		}
	}
	nodes, err := d.st.Nodes()
	if err != nil {
		return doctor.ApplianceFacts{}, err
	}
	for _, n := range nodes {
		if n.Kind != string(pki.KindAppliance) || n.VMID == 0 || n.State == store.NodeRemoved {
			continue
		}
		a := doctor.AgentContainer{Node: n.Name, VMID: n.VMID, HA: isHA(n.VMID), Cmode: "tty"}
		ref := model.GuestRef{Kind: model.KindLXC, VMID: n.VMID}
		if i := slices.IndexFunc(res, func(r pve.Resource) bool { return r.Kind == model.KindLXC && r.VMID == n.VMID }); i >= 0 {
			a.Found, a.RunsOn, a.Lock = true, res[i].Node, res[i].Lock
			cfg, err := d.px.GuestConfig(ctx, a.RunsOn, ref)
			switch {
			case err != nil:
				a.Err = err.Error()
			case cfg.Values["cmode"] != "":
				a.Cmode = cfg.Values["cmode"]
			}
		}
		f.Agents = append(f.Agents, a)
	}
	return f, nil
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
