# Cluster Recommendation Flow

## API Entry Point

| Method | Path | Input |
|--------|------|-------|
| `GET`  | `/api/cluster-recommendation` | Query params: `cluster`, `vmName`, `vmNamespace` |
| `POST` | `/api/cluster-recommendation` | Same fields as JSON body |

**GET example:**
```
GET /api/cluster-recommendation?cluster=managed1&vmName=my-vm&vmNamespace=default
```

Storage class names and disk sizes are derived automatically from the running
`VirtualMachineInstance` status. The caller does not need to supply them.

---

## High-Level Flow

```mermaid
flowchart TD
    A([HTTP Request]) --> B["Validate: cluster, vmName, vmNamespace required"]
    B --> FETCH["fetchVMRequirements via cluster-proxy to source cluster"]
    FETCH --> VMI["GET VirtualMachineInstance (already has resolved CPU, memory, PVC info)"]
    VMI --> CPU["Extract CPU: spec.domain.cpu sockets x cores x threads (each defaults to 1)"]
    VMI --> MEM["Extract Memory: status.memory.guestAtBoot (fallback: spec.domain.memory.guest)"]
    VMI --> VOL["Walk status.volumeStatus"]
    VOL --> PVCVOL["persistentVolumeClaimInfo present: claimName + capacity from VMI status"]
    PVCVOL --> GETSC["Fetch PVC (once per disk) only to read storageClassName"]
    VOL --> HD["hostDisk (detected via spec.volumes): add warning, skip"]
    VOL --> OTHER["cloudinitdisk, containerDisk, etc.: no persistentVolumeClaimInfo, silently skip"]
    GETSC --> REQ["VMResourceRequirements (cpuCores, memoryGiB, volumes)"]
    REQ --> D["getEligibleManagedClusters (label: acm/cnv-operator-install=true)"]
    D --> E{Clusters found?}
    E -- No --> F([Response: error])
    E -- Yes --> G["Score each cluster in parallel (30s timeout)"]
    G --> H{MSA token ready?}
    H -- No --> WARN["Log warning: skip cluster (OCM still provisioning)"]
    H -- Yes --> SCORE[scoreCluster]
    SCORE --> SORT[Sort by TotalScore DESC]
    SORT --> PICK[Pick first CanFitVM=true]
    PICK --> J([Response: RecommendedCluster + AllClusters + Warnings])
```

---

## VMI Volume Inspection

Storage info is read from `status.volumeStatus` in the `VirtualMachineInstance`.
Each entry that has a `persistentVolumeClaimInfo` block is a PVC-backed disk.
The VMI status already contains the actual `claimName` and `capacity` — no DataVolume fetch is needed.
The PVC is fetched once per disk only to read its `storageClassName`.

| `status.volumeStatus` entry | `persistentVolumeClaimInfo`? | Action |
|---|---|---|
| rootdisk, data disks (DataVolume or PVC backed) | Yes | Read `claimName` + `capacity.storage` from VMI; fetch PVC for `storageClassName` |
| `hostDisk` (detected from `spec.volumes`) | No | Cannot determine size → warning added to response |
| `cloudinitdisk`, `containerDisk`, `configMap`, `secret`, etc. | No | No persistent disk to migrate — silently skipped |

**Why VMI instead of VM?**

When a VM uses a `ClusterInstanceType` (e.g. `u1.medium`), the VM spec has `resources: {}`
with no CPU or memory values. The VMI has these already resolved by the time it is Running:

| Field | VM path | VMI path |
|---|---|---|
| CPU | `spec.template.spec.domain.cpu.{sockets,cores,threads}` (may be empty) | `spec.domain.cpu.{sockets,cores,threads}` (always set) |
| Memory | `spec.template.spec.domain.memory.guest` (may be empty) | `status.memory.guestAtBoot` → `spec.domain.memory.guest` |
| Disk sizes | Must fetch each DataVolume or PVC | `status.volumeStatus[*].persistentVolumeClaimInfo.capacity` |

---

## scoreCluster Detail

```mermaid
flowchart TD
    SC[scoreCluster] --> NR["getSchedulableNodeResources (free CPU + Memory per node)"]
    SC --> ST["getStorageClasses (one per unique storageClassName in volumes)"]
    SC --> CALC[calculateResourceScores]
    SC --> FIT[canFitVM]

    NR --> PARALLEL["Two parallel calls via cluster-proxy"]
    PARALLEL --> NODES["GET /api/v1/nodes?labelSelector=kubevirt.io/schedulable=true (filter: Ready)"]
    PARALLEL --> METRICS["GET /apis/metrics.k8s.io/v1beta1/nodes (oc adm top nodes equivalent)"]
    NODES --> FREE["free CPU = Allocatable - actual_usage, free Mem = Allocatable - actual_usage"]
    METRICS --> FREE

    ST --> PROXY2["cluster-proxy: StorageV1.StorageClasses.Get (per unique class)"]
    ST --> CEPH{"Ceph provisioner? (contains ceph or rbd)"}
    CEPH -- Yes --> CC["getCephCapacity: CephCluster CR bytesAvailable (fetched once, shared across all Ceph volumes)"]
    CEPH -- No --> STATIC{"Static provisioner? (kubernetes.io/no-provisioner, local.csi.k8s.io)"}
    STATIC -- Yes --> PV["getAvailablePVSizes: list individual Available PV sizes (CapacityKnown=true, 1:1 matching per volume)"]
    STATIC -- No --> DYN["Other dynamic (topolvm, NFS, cloud CSI): CapacityKnown=false, storage not scored"]
```

---

## cluster-proxy Authentication (per cluster, per request)

```mermaid
flowchart TD
    CFG[newClusterProxyConfig] --> HOST{DEV_MODE=true?}
    HOST -- Yes --> ROUTE["getClusterProxyHostFromRoute: OpenShift Route spec.host (TLS skipped)"]
    HOST -- No --> SVC["getClusterProxyHostFromService: cluster-proxy-addon-user.multicluster-engine.svc:9092 (TLS: openshift-service-ca.crt)"]

    CFG --> MSA[getMSAToken]
    MSA --> ENSURE["ensureManagedServiceAccount (create if not exists)"]
    ENSURE --> CP["ensureClusterPermission: ClusterPermission on hub -> cluster-admin SA on managed cluster"]
    MSA --> SECRET["Read token from MSA status.tokenSecretRef"]
    SECRET --> READY{Token ready?}
    READY -- No --> ERR["errMSATokenNotReady: log warning, skip cluster"]
    READY -- Yes --> TOKEN[Bearer token for cluster-proxy]
```

---

## Scoring Formula

```
Node selection:   best single node (max free CPU + free Memory)
                  VM lands on ONE node — total cluster sum is irrelevant

CPUScore          = freeNodeCPU  / requiredCPU      (ratio, no cap — higher = more headroom)
MemoryScore       = freeNodeMem  / requiredMemory   (ratio, no cap)

StorageScore      = totalAvailableGB / totalRequiredGB   for classes where CapacityKnown=true
                    (Ceph, static PVs — volumes grouped by class, totals compared)
                    excluded from TotalScore              for classes where CapacityKnown=false
                    (NFS, cloud CSI, topolvm — capacity not introspectable)

TotalScore        = (CPU + Memory + Storage) / 3    when any class has CapacityKnown=true
                  = (CPU + Memory) / 2              when all classes have CapacityKnown=false

CanFitVM          = any node has freeCPU >= required
                  AND any node has freeMem >= required
                  AND for each storageClass used by a volume:
                    Dynamic + CapacityKnown=false  -> always passes
                    Dynamic + CapacityKnown=true   -> sum(volumes using this class) <= available
                    Static  + CapacityKnown=true   -> each volume matched 1:1 to an Available PV
                                                      (PV size >= volume size, one PV per volume)
                    Class missing on target        -> fails
```

---

## Free Space Calculation

| Resource | Source | Formula |
|---|---|---|
| **CPU** | `GET /apis/metrics.k8s.io/v1beta1/nodes` | `node.Allocatable.CPU - metrics.usage.CPU` |
| **Memory** | `GET /apis/metrics.k8s.io/v1beta1/nodes` | `node.Allocatable.Memory - metrics.usage.Memory` |
| **Ceph storage** | `CephCluster CR status.ceph.capacity.bytesAvailable` | Direct (already free bytes) — fetched once per cluster |
| **Static PVs** | `GET /api/v1/persistentvolumes` field-filtered | Individual PV sizes listed — each volume matched 1:1 to a PV with size >= volume size |
| **topolvm / LVMS** | Not measurable (node VG) | VG free space not accessible via Kubernetes API |
| **NFS / cloud CSI** | Not measurable | Backend pool is virtually unlimited — no capacity check |

---

## Hub Resources Created Per Cluster (once, on first request)

| Resource | Kind | Namespace | Purpose |
|---|---|---|---|
| `cluster-recommendation` | `ManagedServiceAccount` | `<clusterName>` | Creates SA + rotated token on managed cluster |
| `cluster-recommendation` | `ClusterPermission` | `<clusterName>` | Binds SA to `cluster-admin` on managed cluster |

---

## Storage Class Behaviour by Provisioner

| Provisioner | `IsDynamic` | `CapacityKnown` | Eligibility check | Storage score | TotalScore |
|---|---|---|---|---|---|
| `openshift-storage.rbd.csi.ceph.com` (ODF/Ceph) | true | true | `sum(volumes) <= available` | `available / sum(volumes)` | `(CPU + Mem + Storage) / 3` |
| `rook-ceph.rbd.csi.ceph.com` (Rook) | true | true | `sum(volumes) <= available` | `available / sum(volumes)` | `(CPU + Mem + Storage) / 3` |
| `kubernetes.io/no-provisioner` (local static PVs) | false | true | each volume matched 1:1 to an Available PV (PV size >= volume size) | `totalAvailablePVs / sum(volumes)` | `(CPU + Mem + Storage) / 3` |
| `local.csi.k8s.io` (sig-storage local static CSI) | false | true | each volume matched 1:1 to an Available PV (PV size >= volume size) | `totalAvailablePVs / sum(volumes)` | `(CPU + Mem + Storage) / 3` |
| `topolvm.io` / `lvm.topolvm.io` (LVMS) | true | false | always passes | not scored | `(CPU + Mem) / 2` |
| `nfs.csi.k8s.io` (NFS CSI) | true | false | always passes | not scored | `(CPU + Mem) / 2` |
| `ebs.csi.aws.com` / `disk.csi.azure.com` / `pd.csi.storage.gke.io` | true | false | always passes | not scored | `(CPU + Mem) / 2` |
| `csi.vsphere.vmware.com` | true | false | always passes | not scored | `(CPU + Mem) / 2` |
| `driver.longhorn.io` | true | false | always passes | not scored | `(CPU + Mem) / 2` |
| Unknown + contains `csi` | true | false | always passes | not scored | `(CPU + Mem) / 2` |
| Unknown + no `csi` in name | false | true (0 GB if no PVs) | each volume matched 1:1 to an Available PV | `totalAvailablePVs / sum(volumes)` | `(CPU + Mem + Storage) / 3` |
| **Missing on target cluster** | — | — | **fails immediately** | — | — |

---

## Storage Class Decision Tree

```mermaid
flowchart TD
    SC["storageClassName (from PVC fetched via VMI status.volumeStatus)"] --> CEPH{"Ceph provisioner? (contains ceph or rbd)"}
    CEPH -- Yes --> CEPHQ["Query CephCluster CR once: bytesAvailable -> CapacityKnown=true, shared across all Ceph volumes"]
    CEPH -- No --> STAT{"Static provisioner? (kubernetes.io/no-provisioner, local.csi.k8s.io)"}
    STAT -- Yes --> PVQ["List individual Available PV sizes -> CapacityKnown=true, each volume matched 1:1 to a PV"]
    STAT -- No --> DYN["Other dynamic (topolvm, NFS, cloud CSI): CapacityKnown=false, storage excluded from TotalScore"]
```
