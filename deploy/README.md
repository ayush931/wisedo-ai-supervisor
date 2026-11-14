# Deployment status

The independent AI Supervisor deployment is planned under WIS-290 and qualified by
WIS-281. No existing MES, gateway, database, identity service or broker is required
by the product architecture. See [the architecture](../docs/independent-supervisor.md).

The multi-stage Dockerfile builds and runs the independent Go AI Supervisor binary
with its embedded frontend assets. `make image` builds
`ghcr.io/wisedo-technology/wisedo-ai-supervisor:local`; this does not publish a
registry image. Local PostgreSQL, Keycloak, and Temporal dependencies are defined
only in [`deploy/supervisor/compose.yml`](supervisor/compose.yml); Redis and the
retired OEE Compose stack are not runtime dependencies.

## AI Training Infrastructure (Terraform .tf)

The offline AI training platform (`deploy/aws/`) has been converted from Python-based provisioning into declarative **Terraform (`.tf`)** Infrastructure-as-Code:

- **Declarative Modules:** Standardized on Terraform `.tf` (`main.tf`, `iam.tf`, `network.tf`, `outputs.tf`, `variables.tf`, `versions.tf`), eliminating imperative Python infrastructure scripts.
- **Managed Resources:** S3 data lake & checkpoint buckets (SSE-KMS), SageMaker training jobs, ECR container registries, private VPC endpoints, and least-privilege IAM roles.
- **Workload Separation:** Python is retained solely for containerized workload execution (data preprocessing and QLoRA/LoRA training in `train.py`) and job parameter passing (`launch_training.py`), while all underlying cloud infrastructure lifecycle is 100% managed via Terraform `.tf`.
- **Relationship to Supervisor:** The training infrastructure is offline and optional; the independent Go AI Supervisor runtime does not require SageMaker or AWS training resources to execute.
