# Use cases

Six setups, each from the situation to the settings and Notes it ends with. They differ in
who runs the node, who may publish, and what is in front of the tunnel. Pick the one that is
closest to yours, and read the others for the decisions they explain.

| Page | The situation | Profile |
|---|---|---|
| [Homelab](homelab.md) | One node at home, one person, a dashboard, a photo library and a NAS. | host |
| [Company cluster](company-cluster.md) | A cluster that several teams use and one team administers. pco runs on one node of it. | host |
| [Hosting tenants](hosting-tenants.md) | Customers publish services from their own machines, and nothing is installed on the hypervisors. | appliance |
| [Internal tools behind Cloudflare Access](internal-tools-behind-access.md) | Tools only staff should reach, with Access in front of pco. | either |
| [Preview environments](preview-environments.md) | A guest for each pull request, made and destroyed by CI. | host |
| [Leaving hand-run tunnels](leaving-hand-run-tunnels.md) | `cloudflared` processes started by hand, moved to pco one name at a time. | either |

Each page has the situation, a diagram of the setup, the decisions in the same order, what
the setup ends with, what to watch once it runs, and the guides it uses. The decisions are the
profile, the admission mode, the identity minimum, the tokens, the egress filter and who may
reach the web interface.

Two facts are behind all six. pco runs on one node, and it does not fail over: while that node
is down, every published hostname is down, and moving pco to another node is something an
admin does. And pco is never in the data path, so the connectors keep serving while the daemon
is stopped or upgraded ([Architecture](../architecture.md)).

The pages show output of the commands as they print it, with example names: `example.com`
and `example.net`, addresses from the ranges meant for documentation and `10.0.0.0/8`, and
machine numbers from 100 to 199.
