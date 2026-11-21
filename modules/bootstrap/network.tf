# One security group for Lambdas and MicroVM ENIs: no ingress, HTTPS out only.
resource "aws_security_group" "runners" {
  name        = "${local.prefix}-runners"
  description = "MicroVM runners and control Lambdas: HTTPS egress only"
  vpc_id      = var.vpc_id
  tags        = merge(local.tags, { Name = "${local.prefix}-runners" })
}

resource "aws_vpc_security_group_egress_rule" "https_cidr" {
  for_each          = toset(var.https_egress_cidrs)
  security_group_id = aws_security_group.runners.id
  description       = "HTTPS to central endpoints and central egress"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv4         = each.value
}

resource "aws_vpc_security_group_egress_rule" "https_prefix_list" {
  for_each          = toset(var.https_egress_prefix_list_ids)
  security_group_id = aws_security_group.runners.id
  description       = "HTTPS to managed prefix list"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  prefix_list_id    = each.value
}

# ---------------------------------------------------------------- MicroVM egress connector

data "aws_iam_policy_document" "operator_trust" {
  # checkov:skip=CKV_AWS_358:not a web identity trust.
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type = "Service"
      # As documented by AWS. CreateNetworkConnector rejects this role when the
      # trust has an aws:SourceAccount condition (verified in the sandbox), so
      # there is none: the role only carries the managed operator policy.
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "operator" {
  name                 = "${local.prefix}-connector-operator"
  assume_role_policy   = data.aws_iam_policy_document.operator_trust.json
  permissions_boundary = var.permissions_boundary_arn
  tags                 = local.tags
}

resource "aws_iam_role_policy_attachment" "operator" {
  role       = aws_iam_role.operator.name
  policy_arn = "arn:${local.partition}:iam::aws:policy/AWSLambdaNetworkConnectorOperatorPolicy"
}

# All MicroVM traffic leaves through this VPC, so it follows the central
# routing, inspection and DNS of the landing zone. INTERNET_EGRESS is never used.
resource "aws_lambdacore_network_connector" "this" {
  name          = "${local.prefix}-egress"
  operator_role = aws_iam_role.operator.arn
  configuration {
    vpc_egress_configuration {
      associated_compute_resource_types = ["MicroVm"]
      network_protocol                  = "IPv4"
      security_group_ids                = [aws_security_group.runners.id]
      subnet_ids                        = var.subnet_ids
    }
  }
  depends_on = [aws_iam_role_policy_attachment.operator]
}

# ---------------------------------------------------------------- Local VPC endpoints (optional)

resource "aws_security_group" "endpoints" {
  count       = var.vpc_endpoints == "local" ? 1 : 0
  name        = "${local.prefix}-endpoints"
  description = "Interface endpoints used by MicroVM runners"
  vpc_id      = var.vpc_id
  tags        = merge(local.tags, { Name = "${local.prefix}-endpoints" })
}

resource "aws_vpc_security_group_ingress_rule" "endpoints" {
  count                        = var.vpc_endpoints == "local" ? 1 : 0
  security_group_id            = aws_security_group.endpoints[0].id
  description                  = "HTTPS from runners"
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
  referenced_security_group_id = aws_security_group.runners.id
}

resource "aws_vpc_security_group_egress_rule" "to_endpoints" {
  count                        = var.vpc_endpoints == "local" ? 1 : 0
  security_group_id            = aws_security_group.runners.id
  description                  = "HTTPS to local endpoints"
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
  referenced_security_group_id = aws_security_group.endpoints[0].id
}

resource "aws_vpc_endpoint" "interface" {
  for_each            = var.vpc_endpoints == "local" ? toset(var.local_endpoint_services) : toset([])
  vpc_id              = var.vpc_id
  service_name        = "com.amazonaws.${local.region}.${each.value}"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = var.subnet_ids
  security_group_ids  = [aws_security_group.endpoints[0].id]
  private_dns_enabled = true
  tags                = merge(local.tags, { Name = "${local.prefix}-${each.value}" })
}

resource "aws_vpc_endpoint" "dynamodb" {
  count             = length(var.dynamodb_gateway_route_table_ids) > 0 ? 1 : 0
  vpc_id            = var.vpc_id
  service_name      = "com.amazonaws.${local.region}.dynamodb"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = var.dynamodb_gateway_route_table_ids
  tags              = merge(local.tags, { Name = "${local.prefix}-dynamodb" })
}
