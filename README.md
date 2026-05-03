# kamatera-autoscaler

A provider-specific Kubernetes node autoscaler for [Kamatera Cloud](https://www.kamatera.com/).
Watches the cluster for `Pending` pods that fail to schedule because of insufficient
CPU/memory, provisions Kamatera VMs through the REST API, and drains idle nodes whose
pool is above its configured `min_nodes`.

> Read this in [Russian / на русском](README.ru.md).

The autoscaler is intentionally **k8s-distribution-agnostic**: it does not know
about k3s, RKE2, kubeadm, or any other installer. Joining the new VM to the cluster
is the job of the cloud-init template **you** supply.

---

## Architecture

```
   ┌────────────────────────────────────────────────────────────┐
   │ kamatera-autoscaler (Deployment, kube-system)              │
   │                                                            │
   │   scaleUp tick (30s)                                       │
   │     1. List Pending pods cluster-wide                      │
   │     2. Group by pool via pool_selector                     │
   │     3. For pools below max_nodes (minus in-flight):        │
   │        POST /service/server  →  poll /service/queue        │
   │                                                            │
   │   scaleDown tick (10m)                                     │
   │     1. List nodes; skip those at pool.min_nodes            │
   │     2. For idle nodes older than idle_threshold:           │
   │        cordon → evict (PDB-safe) → POST /server/terminate  │
   │     3. Delete the K8s Node object                          │
   └─────────────┬───────────────────────────┬──────────────────┘
                 │                           │
        ┌────────▼────────┐           ┌──────▼───────────┐
        │ Kubernetes API  │           │ Kamatera REST    │
        │ (in-cluster or  │           │ cloudcli.        │
        │  KUBECONFIG)    │           │ cloudwm.com      │
        └─────────────────┘           └──────────────────┘
```

**Scale-up flow.** A pod is created with a `nodeSelector`. K8s scheduler can't
place it (insufficient CPU/memory). Within ~30 seconds the autoscaler matches the
pod's selector against a configured pool, calls Kamatera to create a VM with the
pool's cloud-init, and tracks it as in-flight. Once the VM joins the cluster
(typically 2–3 minutes total — VM boot + cloud-init), the scheduler binds the pod.

**Scale-down flow.** Every 10 minutes the autoscaler checks each node's pool. If
the pool is above `min_nodes` and the node has no non-DaemonSet workload pods,
the node is cordoned, drained (respecting `PodDisruptionBudgets`), the underlying
VM is terminated via the REST API, and the K8s Node object is deleted.

### Why two Kamatera APIs

Kamatera exposes two different REST APIs that overlap in functionality but
differ in shape and reliability. The autoscaler uses **both at the same time**:

| API | Auth | Used for | Why |
| --- | --- | --- | --- |
| `cloudcli.cloudwm.com` | `AuthClientId` + `AuthSecret` headers | `POST /service/server` (create), `POST /service/server/terminate` | Only this API has the `script-file` field, which is how we inject cloud-init for k3s/RKE2/etc. join. The console API has no equivalent. |
| `console.kamatera.com/service` | Bearer token (POST `/authenticate`) | `GET /queue` (batched recent commands), `GET /servers` (server list) | Cloudcli's `/service/queue` always returns `[]` regardless of activity, and `/service/server/info` periodically 500s. The console-API counterparts work reliably and let us batch K concurrent waiters into a single round-trip per poll cycle (avoiding N+1 queue/info calls). |

The same `KAMATERA_API_CLIENT_ID` / `KAMATERA_API_SECRET` work for both — the
difference is just how they're presented to each endpoint. Operators don't
need separate credentials.

---

## Quickstart

Pull the prebuilt image (multi-arch amd64 + arm64) once a release is published:

```sh
docker pull ghcr.io/georges1995/kamatera-autoscaler:latest
```

A minimal manifest set looks like this. Place it in your infra repo, not here.

```yaml
apiVersion: v1
kind: ServiceAccount
metadata: { name: kamatera-autoscaler, namespace: kube-system }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: { name: kamatera-autoscaler }
rules:
  - apiGroups: [""]
    resources: ["pods", "nodes"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["delete", "patch", "update"]
  - apiGroups: [""]
    resources: ["pods/eviction"]
    verbs: ["create"]
  - apiGroups: ["policy"]
    resources: ["poddisruptionbudgets"]
    verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: { name: kamatera-autoscaler }
subjects:
  - kind: ServiceAccount
    name: kamatera-autoscaler
    namespace: kube-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kamatera-autoscaler
---
apiVersion: v1
kind: Secret
metadata: { name: kamatera-creds, namespace: kube-system }
stringData:
  KAMATERA_API_CLIENT_ID: "..."
  KAMATERA_API_SECRET: "..."
  AUTOSCALER_JOIN_TOKEN: "..."
  SSH_PUB_KEY: "ssh-ed25519 AAAA..."
---
apiVersion: v1
kind: ConfigMap
metadata: { name: kamatera-autoscaler-config, namespace: kube-system }
data:
  pools.yaml: |
    datacenter: EU-FR
    server_ip: 10.0.0.20
    cloud_init_template: /etc/autoscaler/cloud-init.tpl
    pools:
      - name: general
        cpu_type: B
        cpu_cores: 2
        ram_mb: 4096
        disk_gb: 40
        image: ubuntu_server_24.04_64-bit
        min_nodes: 1
        max_nodes: 4
        node_labels: pool=general
        node_taints: ""
        pod_selector: pool=general
  cloud-init.tpl: |
    #cloud-config
    # ... your cloud-init template here, see examples/cloud-init.tpl
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: kamatera-autoscaler, namespace: kube-system }
spec:
  replicas: 1
  selector: { matchLabels: { app: kamatera-autoscaler } }
  template:
    metadata: { labels: { app: kamatera-autoscaler } }
    spec:
      serviceAccountName: kamatera-autoscaler
      containers:
        - name: autoscaler
          image: ghcr.io/georges1995/kamatera-autoscaler:latest
          envFrom:
            - secretRef: { name: kamatera-creds }
          env:
            - { name: AUTOSCALER_CONFIG, value: /etc/autoscaler/pools.yaml }
            - { name: LOG_LEVEL, value: info }
          volumeMounts:
            - { name: cfg, mountPath: /etc/autoscaler }
          resources:
            requests: { cpu: 50m, memory: 64Mi }
            limits:   { cpu: 200m, memory: 128Mi }
      volumes:
        - name: cfg
          configMap: { name: kamatera-autoscaler-config }
```

> **Note:** these manifests are an illustrative starting point — they are NOT
> shipped in this repo. Keep your real manifests in your infra repo under
> source control with the rest of your cluster bootstrap.

---

## Configuration

### `pools.yaml`

| Field | Type | Description |
| --- | --- | --- |
| `datacenter` | string | Kamatera datacenter (e.g. `EU-FR`, `EU`, `US-NY2`). |
| `vlan_name` | string | Optional private VLAN name. If set, every VM gets a second NIC on this VLAN. |
| `server_ip` | string | The control-plane IP your cloud-init connects new agents to (`{{ .ServerIP }}`). |
| `cloud_init_template` | string | Filesystem path to your cloud-init template. |
| `pools[]` | list | One entry per node pool. |
| `pools[].name` | string | Unique pool identifier; used as the K8s label `pool=<name>`. |
| `pools[].cpu_type` | string | `A` / `B` / `D` / `T`. See Kamatera CPU classes. |
| `pools[].cpu_cores` | int | Number of vCPU. The wire format is e.g. `"2B"`. |
| `pools[].ram_mb` | int | RAM in MB. |
| `pools[].disk_gb` | int | Primary disk in GB. |
| `pools[].image` | string | Kamatera image identifier. |
| `pools[].min_nodes` | int | Lower bound — the autoscaler will not drain below this. |
| `pools[].max_nodes` | int | Upper bound — the autoscaler will not provision above this. |
| `pools[].node_labels` | string | Comma-separated `key=value` labels applied to the node via cloud-init. |
| `pools[].node_taints` | string | Comma-separated `key=value:Effect` taints applied via cloud-init. |
| `pools[].pod_selector` | string | A label selector matched against pod `nodeSelector` to decide which pool should grow. |

### Environment variables

The binary is configured exclusively through environment variables. It does not
read `.env` files; the Makefile target `make run-local` does the sourcing for
local development.

| Name | Required | Description |
| --- | --- | --- |
| `KAMATERA_API_CLIENT_ID` | yes | Kamatera REST API client id. |
| `KAMATERA_API_SECRET` | yes | Kamatera REST API secret. |
| `AUTOSCALER_JOIN_TOKEN` | yes | Cluster join token, surfaced to your template as `{{ .JoinToken }}`. |
| `SSH_PUB_KEY` | yes | SSH public key injected into every provisioned VM. |
| `AUTOSCALER_CONFIG` | no | Path to `pools.yaml` (default `/etc/autoscaler/pools.yaml`). |
| `LOG_LEVEL` | no | `debug`, `info`, `warn`, `error` (default `info`). |
| `KUBECONFIG` | no | Used only outside a cluster; in-Pod auth is auto-detected. |

The YAML loader **rejects** any of the following keys appearing in `pools.yaml`:
`kamatera_secret`, `kamatera_client_id`, `api_secret`, `api_client_id`,
`join_token`, `k3s_token`, `ssh_pub_key`, `ssh_key`, `creds`, `credentials`,
`secrets`. This is a guardrail against committing credentials in cluster config.

### Cloud-init template

The template is parsed at startup with Go `text/template`. Available variables:

| Variable | Source |
| --- | --- |
| `{{ .ServerIP }}` | `config.server_ip` |
| `{{ .JoinToken }}` | `AUTOSCALER_JOIN_TOKEN` env var |
| `{{ .NodeLabels }}` | `pool.node_labels` for the pool being scaled up |
| `{{ .NodeTaints }}` | `pool.node_taints` for the pool being scaled up |

A working example for k3s lives in [`examples/cloud-init.tpl`](examples/cloud-init.tpl).
Adjust to your distribution; the autoscaler doesn't care what the template does as
long as the new VM eventually joins the cluster with the expected labels.

---

## Building from source

```sh
make build         # produces bin/autoscaler
make image         # builds a local kamatera-autoscaler:dev Docker image
make test          # go test -race ./...
make lint          # golangci-lint, falls back to go vet
```

## Running locally

```sh
cp .env.example .env
$EDITOR .env       # fill in real values; .env is gitignored
export KUBECONFIG=~/.kube/config
make run-local
```

The autoscaler will connect to the cluster pointed to by `KUBECONFIG`, read its
config from `examples/pools.yaml` by default, log a startup banner, and tick
every 30 seconds. Use Ctrl-C to shut it down — it terminates within a few
seconds via `signal.NotifyContext`.

---

## Releases & Docker images

A multi-arch (amd64 + arm64) image is published to GHCR **only on semver tags**:

```
ghcr.io/<owner>/kamatera-autoscaler:v1.2.3
ghcr.io/<owner>/kamatera-autoscaler:v1.2
ghcr.io/<owner>/kamatera-autoscaler:v1
ghcr.io/<owner>/kamatera-autoscaler:latest
```

Push to `main` does **not** publish images — this is intentional. Cut a release with:

```sh
git tag v0.1.0
git push --tags
```

### Pulling from a private GHCR package

By default a GHCR package inherits the visibility of the publishing repo. While
private, you can still consume it from your own systems:

**From another GitHub Actions workflow** (in any repo of the same owner/org):

```yaml
permissions:
  packages: read
steps:
  - run: echo "$GITHUB_TOKEN" | docker login ghcr.io -u "$GITHUB_ACTOR" --password-stdin
    env:
      GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
  - run: docker pull ghcr.io/<owner>/kamatera-autoscaler:v0.1.0
```

**From a Kubernetes cluster** — create an `imagePullSecret`:

1. Create a GitHub PAT with the `read:packages` scope.
2. Register it as a Kubernetes pull secret:
   ```sh
   kubectl create secret docker-registry ghcr-pull \
     --docker-server=ghcr.io \
     --docker-username=<your-gh-username> \
     --docker-password=<the-PAT> \
     --docker-email=<your-email> \
     -n kube-system
   ```
3. Reference it in the Deployment:
   ```yaml
   spec:
     template:
       spec:
         imagePullSecrets:
           - name: ghcr-pull
   ```

### Making the image public

GitHub UI → repository → Packages → `kamatera-autoscaler` → Package settings →
Change visibility → Public. Anyone can then `docker pull` it without auth.

---

## Testing

### Unit tests

```sh
make test
```

The fake `kubernetes.Interface` (from `client-go/kubernetes/fake`) and the
`httptest` server give the controller deterministic test scenarios for both
scale-up and scale-down without touching real infrastructure.

### Native E2E (real Kamatera, ~$0.01 per run)

The end-to-end test against the live Kamatera API is **not** part of this repo
(it lives under `test/e2e/`, which is gitignored, so credentials and real-VM
side effects can't be accidentally committed). Generate the files locally and
run them when you want to validate REST changes against the real API. See the
plan / spec for the exact safeguards (env-var checks, `KAMATERA_E2E_CONFIRM`,
forced cleanup, orphan-cleanup script).

---

## Project layout

```
cmd/autoscaler/         entrypoint
internal/config/        YAML schema, validation, env-only credentials
internal/kamatera/      REST client (auth, retries, queue polling)
internal/kubeclient/    interfaces wrapping client-go (testable)
internal/controller/    scale_up, scale_down, drain, pool matching, in-flight tracker
internal/logging/       slog JSON handler with secret-redacting attribute replacer
examples/               sample pools.yaml + cloud-init.tpl
.github/workflows/      ci.yml + release.yml (GHCR publish on semver tags)
```

The `pkg/` directory is intentionally absent — there is no public Go API.

---

## License

[MIT](LICENSE).
