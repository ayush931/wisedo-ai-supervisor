# ==============================================================================
# Private networking: VPC endpoints so SageMaker training needs no NAT/IGW
# (zero-egress per ADR 0004 Sec. 7/9). Created only when a VPC is supplied.
# ==============================================================================

data "aws_vpc" "training" {
  count = local.in_vpc ? 1 : 0
  id    = var.vpc_id
}

data "aws_route_tables" "training" {
  count  = local.in_vpc && var.create_vpc_endpoints ? 1 : 0
  vpc_id = var.vpc_id
}

# Regional S3 gateway prefix list (only needed when endpoints are managed).
data "aws_prefix_list" "s3" {
  count = local.in_vpc && var.create_vpc_endpoints ? 1 : 0
  name  = "com.amazonaws.${var.aws_region}.s3"
}

resource "aws_security_group" "sagemaker_training" {
  count       = local.in_vpc ? 1 : 0
  name        = "${local.name_prefix}-sagemaker"
  description = "SageMaker training ENIs: no ingress; egress limited to the VPC (endpoints/DNS/nodes) plus S3."
  vpc_id      = var.vpc_id
  tags        = local.tags

  # No ingress rules: training nodes never accept inbound connections.
  # A Terraform-managed SG has NO default allow-all egress, so egress must be
  # explicit — otherwise training nodes cannot reach ECR, S3, CloudWatch Logs,
  # or each other. All traffic to the VPC CIDR covers the interface endpoints
  # (ECR API, CloudWatch Logs, STS, SageMaker), AmazonProvidedDNS (VPC CIDR+2),
  # and intra-cluster node traffic.
  egress {
    description = "VPC-internal: interface endpoints, DNS, and training nodes"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = [data.aws_vpc.training[0].cidr_block]
  }

  # Endpoints path: S3 is reached via the gateway endpoint (regional prefix
  # list) on 443. No path to the public internet exists in this mode.
  dynamic "egress" {
    for_each = var.create_vpc_endpoints ? [1] : []
    content {
      description     = "S3 via gateway endpoint (regional prefix list)"
      from_port       = 443
      to_port         = 443
      protocol        = "tcp"
      prefix_list_ids = [data.aws_prefix_list.s3[0].prefix_list_id]
    }
  }

  # NAT path (create_vpc_endpoints = false): S3/ECR/etc. over 443 to the
  # internet, assuming a NAT gateway in the private subnets.
  dynamic "egress" {
    for_each = var.create_vpc_endpoints ? [] : [1]
    content {
      description = "HTTPS out via NAT (endpoints disabled)"
      from_port   = 443
      to_port     = 443
      protocol    = "tcp"
      cidr_blocks = ["0.0.0.0/0"]
    }
  }
}

resource "aws_vpc_endpoint" "s3" {
  count             = local.in_vpc && var.create_vpc_endpoints ? 1 : 0
  vpc_id            = var.vpc_id
  service_name      = "com.amazonaws.${var.aws_region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = data.aws_route_tables.training[0].ids
  tags              = local.tags
}

resource "aws_vpc_endpoint" "interfaces" {
  for_each = local.in_vpc && var.create_vpc_endpoints ? toset(var.vpc_endpoint_services) : toset([])

  vpc_id              = var.vpc_id
  service_name        = "com.amazonaws.${var.aws_region}.${each.value}"
  vpc_endpoint_type   = "Interface"
  private_dns_enabled = true
  subnet_ids          = var.private_subnet_ids
  security_group_ids  = [aws_security_group.sagemaker_training[0].id]
  tags                = local.tags
}
