output "scp_json" {
  value = jsonencode(local.scp)
}

output "rcp_json" {
  value = jsonencode(local.rcp)
}

output "tag_policy_json" {
  value = jsonencode(local.tag_policy)
}

output "policy_ids" {
  value = { for k, p in aws_organizations_policy.this : k => p.id }
}
