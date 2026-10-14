package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// maxDoctorAnswer bounds the findings the controller reads of one agent's
// answer, as the agent bounds what it sends.
const maxDoctorAnswer = 256 << 10

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
func (d doctorCluster) CAExpires() (time.Time, error) { return caExpires(d.cp.st) }

func (d doctorCluster) Facts() engine.ClusterFacts { return d.eng.ClusterFacts() }

// Ask has the agents of nodes make their checks through a doctor action,
// and reads their findings.
func (d doctorCluster) Ask(ctx context.Context, nodes []string, wait time.Duration) map[string]doctor.AgentAnswer {
	out := map[string]doctor.AgentAnswer{}
	for node, res := range d.eng.AskDoctor(ctx, nodes, wait) {
		a := doctor.AgentAnswer{Answered: true, Err: res.Error}
		switch {
		case res.Error != "":
		case len(res.Data) > maxDoctorAnswer:
			a.Err = fmt.Sprintf("its findings are %d bytes, over the %d an answer may carry", len(res.Data), maxDoctorAnswer)
		default:
			if err := json.Unmarshal(res.Data, &a.Findings); err != nil {
				a.Err = "its findings cannot be read: " + err.Error()
			}
		}
		out[node] = a
	}
	return out
}

// agentDoctor makes the checks of this node for a doctor action of the
// controller, through the doctor of the daemon once it is made.
type agentDoctor struct {
	runner atomic.Pointer[doctor.Runner]
}

func (a *agentDoctor) set(r *doctor.Runner) { a.runner.Store(r) }

func (a *agentDoctor) run(ctx context.Context) (json.RawMessage, error) {
	r := a.runner.Load()
	if r == nil {
		return nil, errors.New("the daemon of the node is starting")
	}
	return json.Marshal(r.AgentChecks(ctx))
}
