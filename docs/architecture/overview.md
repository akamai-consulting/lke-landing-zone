# Architecture overview

Two views of how the LKE Landing Zone (LLZ) fits together:

- The **[high-level view](#high-level-publish--consume)** — what this repo
  produces and how a downstream instance repo turns it into a running cluster.
- The **[low-level view](#low-level-how-an-instance-converges)** — the concrete
  components inside an instance: the `llz` CLI, the Terraform roots and modules,
  the in-cluster bootstrap chain, and the day-2 workflows.

> These diagrams are a map, not a contract. The load-bearing details live in the
> READMEs they point at and in
> [convergence-contract.md](convergence-contract.md). When code and a diagram
> disagree, the code wins — please fix the diagram.

## High-level: publish → consume

LLZ is a **template repo that publishes immutable, independently consumable
artifacts**. It is *not* a running deployment. A downstream **instance repo**
consumes those artifacts — pinned to one umbrella tag — and the `llz` CLI drives
it from scaffold to a converged LKE-Enterprise cluster and on into day-2.

```mermaid
flowchart LR
    subgraph TPL["📦 Template repo (this repo) — builds & publishes"]
        direction TB
        TF["terraform-modules/<br/>llz-cluster · llz-pool · llz-node-firewall<br/>llz-object-storage · llz-openbao"]
        CH["kubernetes-charts/<br/>5 first-party Helm charts"]
        IMG["dockerfiles/<br/>ci-terraform · ci-kubernetes · devcontainer"]
        CLI["tools/<br/>the llz CLI (Go)"]
        WF["reusable workflows<br/>+ instance-template/ scaffold"]
    end

    subgraph REG["🏷️ Published artifacts"]
        direction TB
        GTAG["git:: umbrella tag<br/>vX.Y.Z (modules + workflows + CLI)"]
        OCI["GHCR OCI charts<br/>independently versioned"]
        GHCRimg["GHCR container images"]
    end

    subgraph INST["🏗️ Instance repo (downstream, per org)"]
        direction TB
        IDENT["org/cluster identity only<br/>tfvars + apl-values overlays"]
        STUBS["thin caller stubs<br/>(reusable workflows @vX.Y.Z)"]
        PINBIN["pinned llz binary"]
    end

    TARGET["☸️ LKE-Enterprise cluster<br/>+ Akamai App Platform (apl-core)"]

    TF --> GTAG
    WF --> GTAG
    CLI --> GTAG
    CH --> OCI
    IMG --> GHCRimg

    GTAG -->|"source = git::…?ref=vX.Y.Z"| INST
    OCI -->|"Argo CD targetRevision: X.Y.Z"| TARGET
    GHCRimg -->|"TF_IMAGE / KUBE_IMAGE"| STUBS

    PINBIN -->|"llz new / env add / up"| INST
    INST -->|"scaffold → apply → converge"| TARGET

    classDef repo fill:#e8f0fe,stroke:#4285f4,color:#111;
    classDef art fill:#fef7e0,stroke:#f9ab00,color:#111;
    classDef tgt fill:#e6f4ea,stroke:#34a853,color:#111;
    class TF,CH,IMG,CLI,WF repo;
    class GTAG,OCI,GHCRimg art;
    class TARGET tgt;
```

**Key relationships**

| Edge | Meaning |
|---|---|
| Template → git tag | Modules, reusable workflows, and the `llz` CLI release **lockstep** under one bare SemVer tag `vX.Y.Z`. |
| Template → GHCR OCI | Helm charts are the **exception**: versioned independently via `Chart.yaml`, immutable by convention. |
| Tag/OCI → instance | The instance pins everything; upstream fixes arrive via a **version bump**, not a manual diff. |
| CLI → instance → cluster | The `llz` binary is the version anchor and the single driver of the whole lifecycle. |

## Low-level: how an instance converges

Inside an instance, `llz` drives four Terraform roots, the in-cluster bootstrap
chain hands off to Argo CD, and a set of scheduled workflows keep the cluster
converged on day-2. Every "is it ready?" check honours the
[three-exit-code convergence contract](convergence-contract.md).

```mermaid
flowchart TB
    subgraph DRIVE["llz CLI — drives the lifecycle"]
        direction LR
        NEW["llz new / env add<br/>(copier scaffold)"]
        UP["llz up<br/>credentials → readiness gate → build"]
        DAY2["llz status / health / converge / drift"]
    end

    subgraph TFROOTS["Terraform roots — instance-template/terraform-iac-bootstrap/"]
        direction TB
        R1["cluster<br/>→ llz-cluster (VPC + LKE-E)<br/>→ llz-pool (encrypted nodes + firewall)"]
        R2["object-storage<br/>→ llz-object-storage (OBJ + scoped keys)"]
        R3["openbao-config<br/>→ llz-openbao (KV v2 + AppRole/K8s auth)"]
        R4["cluster-bootstrap<br/>helm_release.apl + readiness gates"]
    end

    subgraph CLUSTER["☸️ In-cluster bootstrap chain (sync-wave ordered)"]
        direction TB
        APL["apl-operator<br/>(helmfile pipeline, ~40 components)"]
        ARGO["Argo CD<br/>application-controller"]
        AOA["llz-argo-bootstrap-apps<br/>(app-of-apps, encodes sync waves)"]
        FOUND["llz-cluster-foundation<br/>namespaces · default-deny NetworkPolicy · CoreDNS"]
        BAO["llz-openbao-platform<br/>(TLS · ServiceMonitor · AppRole rotation)"]
        CERT["llz-cert-automation + llz-eso-cert-watcher<br/>(event-driven cert renewal)"]
    end

    GATE{{"Convergence contract<br/>0 = converged · 2 = in-progress (poll) · 1 = hard-fail (stop)"}}

    NEW --> R1
    UP --> R1 --> R2 --> R3 --> R4
    R4 -->|"helm_release.apl"| APL
    APL --> ARGO
    R4 -.->|"apl_pipeline_ready gate<br/>(controller Available)"| ARGO
    ARGO --> AOA
    AOA --> FOUND
    AOA --> BAO
    AOA --> CERT
    R4 -.->|"bootstrap_application_synced gate"| GATE
    AOA --> GATE
    GATE -->|"exit 2 → poll"| DAY2
    GATE -->|"exit 0"| DAY2

    subgraph SCHED["Scheduled day-2 workflows (reusable, called by instance stubs)"]
        direction LR
        H["cluster-health"]
        ROT["secret-rotation<br/>(lke-admin · Linode PAT · TF-state keys)"]
        UNSEAL["openbao-auto-unseal<br/>(self-healing)"]
        CHK["scheduled-checks"]
    end

    DAY2 --> SCHED
    SCHED -.->|"poll readiness"| GATE
    UNSEAL -.-> BAO

    classDef cli fill:#e8f0fe,stroke:#4285f4,color:#111;
    classDef tf fill:#f3e8fd,stroke:#a142f4,color:#111;
    classDef k8s fill:#e6f4ea,stroke:#34a853,color:#111;
    classDef gate fill:#fce8e6,stroke:#ea4335,color:#111;
    classDef sch fill:#fef7e0,stroke:#f9ab00,color:#111;
    class NEW,UP,DAY2 cli;
    class R1,R2,R3,R4 tf;
    class APL,ARGO,AOA,FOUND,BAO,CERT k8s;
    class GATE gate;
    class H,ROT,UNSEAL,CHK sch;
```

**Reading the bootstrap chain**

1. `llz up` applies the Terraform roots in order; `cluster-bootstrap` installs the
   apl-operator via `helm_release.apl`.
2. The apl-operator runs its helmfile pipeline (~40 components) and stands up
   **Argo CD**. Terraform's `apl_pipeline_ready` gate waits for the
   `argocd-application-controller` StatefulSet to be `Available` before applying
   the bootstrap Application — otherwise it would race the pipeline.
3. The **app-of-apps** (`llz-argo-bootstrap-apps`) fans out the first-party charts
   in **sync-wave order**: foundation (namespaces, default-deny NetworkPolicy)
   before the OpenBao platform and cert automation.
4. `cluster-bootstrap` returns success **only** once
   `bootstrap_application_synced` reports `Synced + Healthy` (or the documented
   deferred-input steady state) — the single "TF has done its job" signal.
5. Day-2 reusable workflows poll the same readiness model and keep the cluster
   converged without standing operator toil.

## See also

- [README — what it ships and how it's published/consumed](../../README.md)
- [Delivery methodology — the phased path these mechanics drive](../delivery-methodology.md)
- [Convergence contract — the three exit codes in detail](convergence-contract.md)
- [Environments as a dev → staging → prod pipeline](../environments-and-promotion.md)
- [terraform-modules/README.md](../../terraform-modules/README.md) ·
  [kubernetes-charts/README.md](../../kubernetes-charts/README.md)
