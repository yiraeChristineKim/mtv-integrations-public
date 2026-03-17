# Cluster Recommendation API — Usage Examples

Find the best managed cluster to migrate a VM to, based on free CPU, memory, and storage.
The API inspects the running `VirtualMachineInstance` on the source cluster via cluster-proxy
and derives all resource requirements automatically.

## API Endpoints

| Method | Path | Input |
|---|---|---|
| `GET` | `/api/cluster-recommendation` | Query parameters |
| `POST` | `/api/cluster-recommendation` | JSON body |

The caller supplies only **where the VM is**. Storage class names and disk sizes are
read automatically from the VMI's `status.volumeStatus`.

---

## Request Examples

### GET
```bash
curl "http://localhost:8090/api/cluster-recommendation?\
cluster=managed1&vmName=my-vm&vmNamespace=default"
```

### POST
```bash
curl -X POST http://localhost:8090/api/cluster-recommendation \
  -H "Content-Type: application/json" \
  -d '{
    "cluster":      "managed1",
    "vmName":       "my-vm",
    "vmNamespace":  "default"
  }'
```

### Parameters

| Parameter | Required | Description |
|---|---|---|
| `cluster` | Yes | Name of the managed cluster where the VM currently runs |
| `vmName` | Yes | `VirtualMachineInstance` name |
| `vmNamespace` | Yes | Namespace of the VMI on the source cluster |

---

## Response — Ceph / ODF storage (CapacityKnown=true)

The VM has 1 vCPU, 4 GiB RAM, and three disks all on `ocs-storagecluster-ceph-rbd`.
`storageScore` is included in `totalScore` because available Ceph bytes are measured.

```json
{
  "recommendedCluster": {
    "clusterName": "prod-cluster",
    "clusterUrl": "https://api.prod-cluster.example.com:6443",
    "totalScore": 8.33,
    "cpuScore": 12.0,
    "memoryScore": 10.0,
    "storageScore": 4.29,
    "schedulableNodes": 3,
    "bestNode": {
      "nodeName": "worker-0.prod-cluster.example.com",
      "availableCpuCores": 12,
      "availableMemoryGiB": 40
    },
    "nodeResources": [
      {
        "nodeName": "worker-0.prod-cluster.example.com",
        "availableCpuCores": 12,
        "availableMemoryGiB": 40
      },
      {
        "nodeName": "worker-1.prod-cluster.example.com",
        "availableCpuCores": 8,
        "availableMemoryGiB": 32
      }
    ],
    "storageClasses": [
      {
        "name": "ocs-storagecluster-ceph-rbd",
        "provisioner": "openshift-storage.rbd.csi.ceph.com",
        "volumeBindingMode": "Immediate",
        "allowVolumeExpansion": true,
        "isDynamic": true,
        "capacityKnown": true,
        "availableCapacityGB": 300
      }
    ],
    "canFitVm": true
  },
  "allClusters": ["..."],
  "vmRequirements": {
    "cpuCores": 1,
    "memoryGiB": 4,
    "volumes": [
      { "volumeName": "rootdisk",          "storageClassName": "ocs-storagecluster-ceph-rbd", "sizeGB": 30 },
      { "volumeName": "from-sub02-orange", "storageClassName": "ocs-storagecluster-ceph-rbd", "sizeGB": 30 },
      { "volumeName": "from-sub02-apple",  "storageClassName": "ocs-storagecluster-ceph-rbd", "sizeGB": 10 }
    ]
  },
  "warnings": [],
  "status": "success"
}
```

> **Score breakdown (total required storage = 30+30+10 = 70 GB):**
> `cpuScore = 12/1 = 12`, `memoryScore = 40/4 = 10`, `storageScore = 300/70 = 4.29`
> `totalScore = (12 + 10 + 4.29) / 3 = 8.76`

---

## Response — Mixed storage classes

The VM has disks on two different storage classes. Both are Ceph (`capacityKnown=true`),
so their available and required GBs are **pooled together** into a single `storageScore`.
`storageScore = (totalAvailable across all scored classes) / (totalRequired across all scored classes)`

```json
{
  "recommendedCluster": {
    "clusterName": "prod-cluster",
    "totalScore": 8.58,
    "cpuScore": 12.0,
    "memoryScore": 10.0,
    "storageScore": 5.625,
    "storageClasses": [
      {
        "name": "ocs-storagecluster-ceph-rbd",
        "provisioner": "openshift-storage.rbd.csi.ceph.com",
        "isDynamic": true,
        "capacityKnown": true,
        "availableCapacityGB": 300
      },
      {
        "name": "ocs-storagecluster-ceph-rbd-virtualization",
        "provisioner": "openshift-storage.rbd.csi.ceph.com",
        "isDynamic": true,
        "capacityKnown": true,
        "availableCapacityGB": 150
      }
    ],
    "canFitVm": true
  },
  "vmRequirements": {
    "cpuCores": 1,
    "memoryGiB": 4,
    "volumes": [
      { "volumeName": "rootdisk",   "storageClassName": "ocs-storagecluster-ceph-rbd",                "sizeGB": 30 },
      { "volumeName": "data-disk",  "storageClassName": "ocs-storagecluster-ceph-rbd-virtualization", "sizeGB": 50 }
    ]
  },
  "warnings": [],
  "status": "success"
}
```

> **Score breakdown (pooled storage):**
> class A: 300 GB available, 30 GB required — class B: 150 GB available, 50 GB required
> `storageScore = (300 + 150) / (30 + 50) = 450 / 80 = 5.625`
> `cpuScore = 12/1 = 12`, `memoryScore = 40/4 = 10`
> `totalScore = (12 + 10 + 5.625) / 3 = 9.21`

---

## Response — Static PVs (kubernetes.io/no-provisioner)

Static storage classes require each volume to be matched 1:1 to an Available PV whose
size is ≥ the volume size. The API lists individual PV sizes and performs greedy matching
(largest volume matched to largest PV first).

```json
{
  "recommendedCluster": {
    "clusterName": "baremetal-cluster",
    "totalScore": 6.5,
    "cpuScore": 8.0,
    "memoryScore": 6.0,
    "storageScore": 5.5,
    "storageClasses": [
      {
        "name": "local-storage",
        "provisioner": "kubernetes.io/no-provisioner",
        "isDynamic": false,
        "capacityKnown": true,
        "availableCapacityGB": 440
      }
    ],
    "canFitVm": true
  },
  "vmRequirements": {
    "cpuCores": 1,
    "memoryGiB": 4,
    "volumes": [
      { "volumeName": "rootdisk",   "storageClassName": "local-storage", "sizeGB": 30 },
      { "volumeName": "data-disk",  "storageClassName": "local-storage", "sizeGB": 50 }
    ]
  },
  "warnings": [],
  "status": "success"
}
```

> **Eligibility check (1:1 PV matching):** If the cluster has Available PVs of sizes
> `[500GB, 100GB, 40GB]` and the VM needs volumes of `[50GB, 30GB]`, the greedy match is:
> 50GB → 100GB ✓, 30GB → 40GB ✓ → `canFitVm = true`
>
> If the cluster only has `[500GB, 20GB]`, then:
> 50GB → 500GB ✓, 30GB → 20GB ✗ → `canFitVm = false`

---

## Response — NFS / Cloud CSI storage (CapacityKnown=false)

`storageScore` is excluded from `totalScore` because backend capacity cannot be measured.
`availableCapacityGB = 0` means "unknown", not "empty" — the cluster still passes eligibility.

```json
{
  "recommendedCluster": {
    "clusterName": "dev-cluster",
    "totalScore": 6.0,
    "cpuScore": 8.0,
    "memoryScore": 4.0,
    "storageScore": 0,
    "storageClasses": [
      {
        "name": "nfs-csi",
        "provisioner": "nfs.csi.k8s.io",
        "isDynamic": true,
        "capacityKnown": false,
        "availableCapacityGB": 0
      }
    ],
    "canFitVm": true
  },
  "vmRequirements": {
    "cpuCores": 1,
    "memoryGiB": 4,
    "volumes": [
      { "volumeName": "rootdisk",  "storageClassName": "nfs-csi", "sizeGB": 30 },
      { "volumeName": "data-disk", "storageClassName": "nfs-csi", "sizeGB": 10 }
    ]
  },
  "warnings": [],
  "status": "success"
}
```

> **Score breakdown:**
> `cpuScore = 32/1 = 32... ` (capped by example), `memoryScore = 16/4 = 4`
> `totalScore = (8 + 4) / 2 = 6.0` ← storage excluded (capacity unknown)

---

## Response — With warnings

Some volume types cannot be inspected (e.g. `hostDisk`). The API still proceeds but
adds a `warnings` entry for each uninspectable volume.

```json
{
  "recommendedCluster": { "...": "..." },
  "vmRequirements": {
    "cpuCores": 2,
    "memoryGiB": 8,
    "volumes": [
      { "volumeName": "rootdisk", "storageClassName": "ocs-storagecluster-ceph-rbd", "sizeGB": 30 }
    ]
  },
  "warnings": [
    "hostDisk volume \"host-data\" is not included in storage calculation"
  ],
  "status": "success"
}
```

---

## Response — No cluster can fit the VM

```json
{
  "recommendedCluster": null,
  "allClusters": [
    {
      "clusterName": "small-cluster",
      "totalScore": 0,
      "cpuScore": 0,
      "memoryScore": 0,
      "storageScore": 0,
      "schedulableNodes": 2,
      "bestNode": {
        "nodeName": "worker-0.small-cluster.example.com",
        "availableCpuCores": 0,
        "availableMemoryGiB": 2
      },
      "storageClasses": [],
      "canFitVm": false
    }
  ],
  "vmRequirements": {
    "cpuCores": 8,
    "memoryGiB": 16,
    "volumes": [
      { "volumeName": "rootdisk", "storageClassName": "ocs-storagecluster-ceph-rbd", "sizeGB": 1000 }
    ]
  },
  "warnings": [],
  "status": "warning",
  "message": "No cluster has sufficient resources to fit the VM"
}
```

---

## Response Fields

### Top-level

| Field | Description |
|---|---|
| `recommendedCluster` | Highest-scoring cluster where `canFitVm=true`. `null` if none qualifies. |
| `allClusters` | All scored clusters sorted by `totalScore` descending |
| `vmRequirements` | Resource requirements extracted from the VMI (echoed back for verification) |
| `warnings` | Non-fatal issues (e.g. hostDisk volumes skipped, PVC storageClass missing) |
| `status` | `"success"`, `"warning"`, or `"error"` |
| `message` | Present on warning/error only |

### vmRequirements fields

| Field | Description |
|---|---|
| `cpuCores` | Total vCPUs: `sockets × cores × threads` from `spec.domain.cpu` |
| `memoryGiB` | Guest memory from `status.memory.guestAtBoot` (fallback: `spec.domain.memory.guest`) |
| `volumes` | One entry per PVC-backed disk. `cloudInitNoCloud`, `containerDisk`, etc. are excluded. |
| `volumes[].volumeName` | Volume name from `status.volumeStatus` |
| `volumes[].storageClassName` | StorageClass name from the PVC on the source cluster |
| `volumes[].sizeGB` | Actual capacity from `status.volumeStatus[].persistentVolumeClaimInfo.capacity.storage` |

### ClusterScore fields

| Field | Description |
|---|---|
| `totalScore` | `(cpu+mem+storage)/3` when any class has `capacityKnown=true`, else `(cpu+mem)/2` |
| `cpuScore` | `freeNodeCPU / requiredCPU` — no cap, higher = more headroom |
| `memoryScore` | `freeNodeMemory / requiredMemory` — no cap |
| `storageScore` | Worst `availableGB / requiredGB` across scored storage classes. `0` when all `capacityKnown=false` |
| `schedulableNodes` | Nodes with `kubevirt.io/schedulable=true` AND `Ready` condition |
| `bestNode` | Node with highest combined free CPU + memory — most likely VM placement target |
| `nodeResources` | Free resources per node: `Allocatable − actual_usage` (from metrics-server) |
| `canFitVm` | `true` if any node has enough free CPU + memory AND all storage checks pass |

### NodeResources fields

| Field | Description |
|---|---|
| `nodeName` | Kubernetes node name |
| `availableCpuCores` | `node.Allocatable.CPU − metrics.usage.CPU` |
| `availableMemoryGiB` | `node.Allocatable.Memory − metrics.usage.Memory` |

### StorageClassInfo fields

| Field | Description |
|---|---|
| `name` | StorageClass name |
| `provisioner` | CSI driver name |
| `isDynamic` | `true` if the provisioner creates PVs on demand |
| `capacityKnown` | `true` only when `availableCapacityGB` is real measured data |
| `availableCapacityGB` | Ceph: `bytesAvailable` from CephCluster CR. Static: sum of `Available` PVs. NFS/cloud: `0` (unknown) |

---

## Scoring Algorithm

### Phase 1 — Eligibility (hard gate)

A cluster is **excluded** (`canFitVm = false`) if any of these fail:
- No single node has `freeCPU ≥ required` (VM runs on one node, not spread)
- No single node has `freeMemory ≥ required`
- For each storage class used by a volume:
  - **Ceph** (`capacityKnown=true`): `sum(volumes using this class) > available` → fails
  - **Static PVs** (`capacityKnown=true`): cannot match each volume 1:1 to an Available PV → fails
  - **Dynamic** (`capacityKnown=false`): always passes
  - **Class missing on target**: fails immediately

### Phase 2 — Scoring (eligible clusters only)

```
freeCPU  = node.Allocatable.CPU    − metrics-server actual usage
freeMem  = node.Allocatable.Memory − metrics-server actual usage

CPUScore     = max(freeCPU  across nodes) / requiredCPU    (unbounded ratio)
MemoryScore  = max(freeMem  across nodes) / requiredMemory (unbounded ratio)
StorageScore = sum(availableGB  for capacityKnown=true classes only)
            / sum(requiredGB    for capacityKnown=true classes only)
              ↑ capacityKnown=false classes (NFS, cloud CSI) always pass eligibility
                but their required GB is NOT counted — they are invisible to this ratio

TotalScore = (CPU + Mem + Storage) / 3   when ANY class has capacityKnown=true
           = (CPU + Mem)           / 2   when ALL classes have capacityKnown=false
```

**Mixed Ceph + cloud CSI example:**
VM has `rootdisk` (30 GB, Ceph) and `data-disk` (50 GB, NFS).
Ceph has 300 GB available.

```
NFS eligibility  → always passes (not checked)
Ceph eligibility → 300 >= 30 ✓

totalRequired  = 30   (NFS 50 GB excluded — capacity unknown)
totalAvailable = 300

storageScore = 300 / 30 = 10.0   ← only Ceph volumes in the ratio
storageKnown = true               ← because at least one class is Ceph
totalScore   = (CPU + Mem + 10.0) / 3
```

> The NFS disk's 50 GB is invisible to `storageScore`. The score can look inflated
> when large disks are on unmeasurable classes. This is a known trade-off: we cannot
> score what we cannot measure, and the NFS class already passed its (always-true) eligibility gate.

Scores are **unbounded ratios** — a node with 12 free cores for a 1-core VM scores 12.
The cluster with the most spare capacity always wins.

---

## Prerequisites

- **ACM** installed on the hub cluster
- **cluster-proxy** addon enabled (for fetching VMI and PVC info from source cluster)
- **Source cluster** must have the VM in `Running` phase (VMI must exist)
- **Target managed clusters** must have:
  - Label `acm/cnv-operator-install=true`
  - CNV (KubeVirt) operator installed
  - Nodes labeled `kubevirt.io/schedulable=true`
  - **metrics-server** running (required for free space calculation)
  - **ODF/Ceph** installed if using Ceph storage classes

### Hub resources created automatically (once per cluster, on first request)

| Resource | Kind | Namespace | Purpose |
|---|---|---|---|
| `cluster-recommendation` | `ManagedServiceAccount` | `<clusterName>` | Creates SA + rotating token on managed cluster |
| `cluster-recommendation` | `ClusterPermission` | `<clusterName>` | Binds SA to `cluster-admin` on managed cluster |

These are created on the hub by the operator. OCM propagates the `ClusterRoleBinding` to the managed cluster automatically.

---

## Storage Class Behaviour by Provisioner

| Provisioner | Type | `capacityKnown` | `availableCapacityGB` | Eligibility check | In `totalScore`? |
|---|---|---|---|---|---|
| `openshift-storage.rbd.csi.ceph.com` | Ceph (ODF) | `true` | CephCluster CR `bytesAvailable` | `sum(volumes) <= available` | Yes |
| `rook-ceph.rbd.csi.ceph.com` | Ceph (Rook) | `true` | CephCluster CR `bytesAvailable` | `sum(volumes) <= available` | Yes |
| `kubernetes.io/no-provisioner` | Static local | `true` | Sum of `Available` PV sizes | Each volume matched 1:1 to an Available PV | Yes |
| `local.csi.k8s.io` | Static CSI | `true` | Sum of `Available` PV sizes | Each volume matched 1:1 to an Available PV | Yes |
| `nfs.csi.k8s.io` | NFS CSI | `false` | `0` (unmeasurable) | Always passes | No |
| `ebs.csi.aws.com` | AWS EBS | `false` | `0` (unmeasurable) | Always passes | No |
| `disk.csi.azure.com` | Azure Disk | `false` | `0` (unmeasurable) | Always passes | No |
| `pd.csi.storage.gke.io` | GCP PD | `false` | `0` (unmeasurable) | Always passes | No |
| `topolvm.io` / `lvm.topolvm.io` | LVMS | `false` | `0` (unmeasurable) | Always passes | No |
| `csi.vsphere.vmware.com` | vSphere | `false` | `0` (unmeasurable) | Always passes | No |
| `driver.longhorn.io` | Longhorn | `false` | `0` (unmeasurable) | Always passes | No |
| **Missing on target cluster** | — | — | — | **Fails immediately** | — |

> **Note on `availableCapacityGB: 0` for NFS/cloud:** This means the capacity is **unknown**, not that storage is full. The cluster still passes eligibility and `canFitVm` can still be `true`. Storage is simply not used as a ranking factor.
>
> **Note on static PVs:** Unlike Ceph (sum-based), static PVs require a **1:1 match** per volume — each volume must find a dedicated Available PV whose size is ≥ the volume size. One PV cannot satisfy two volumes.
