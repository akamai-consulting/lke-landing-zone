output "vpc_id" {
  description = "ID of the shared VPC (for reference; clusters attach by label, not this output)."
  value       = linode_vpc.this.id
}
