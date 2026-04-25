provider "aws" {
  region                      = "us-east-1"
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  access_key                  = "mock_access_key"
  secret_key                  = "mock_secret_key"
}

# --- EC2 Instances (different types) ---
resource "aws_instance" "small" {
  ami           = "ami-674cbc1e"
  instance_type = "t3.micro"

  root_block_device {
    volume_size = 20
  }
}

resource "aws_instance" "large" {
  ami           = "ami-674cbc1e"
  instance_type = "r5.2xlarge"

  root_block_device {
    volume_size = 100
    volume_type = "gp3"
  }

  ebs_block_device {
    device_name = "data"
    volume_type = "gp3"
    volume_size = 500
  }
}

# --- RDS ---
resource "aws_db_instance" "postgres" {
  engine               = "postgres"
  instance_class       = "db.t3.large"
  allocated_storage    = 100
  storage_type         = "gp2"
  multi_az             = true
  skip_final_snapshot  = true
}

resource "aws_db_instance" "mysql_small" {
  engine               = "mysql"
  instance_class       = "db.t3.medium"
  allocated_storage    = 50
  storage_type         = "gp2"
  skip_final_snapshot  = true
}

# --- ELB ---
resource "aws_elb" "web" {
  listener {
    instance_port     = 80
    instance_protocol = "http"
    lb_port           = 80
    lb_protocol       = "http"
  }
  availability_zones = ["us-east-1a"]
}

# --- CloudWatch ---
resource "aws_cloudwatch_log_group" "app_logs" {
  name              = "app-logs"
  retention_in_days = 30
}

# --- EFS ---
resource "aws_efs_file_system" "shared" {
  creation_token = "shared-efs"
}

# --- ElastiCache ---
resource "aws_elasticache_cluster" "redis" {
  cluster_id           = "redis-cluster"
  engine               = "redis"
  node_type            = "cache.m6g.large"
  num_cache_nodes      = 1
  parameter_group_name = "default.redis7"
}

# --- EKS ---
resource "aws_eks_cluster" "main" {
  name     = "main-cluster"
  role_arn = "arn:aws:iam::012345678901:role/eks"

  vpc_config {
    subnet_ids = ["subnet-12345"]
  }
}

# --- SNS ---
resource "aws_sns_topic" "alerts" {
  name = "alerts"
}

# --- SQS ---
resource "aws_sqs_queue" "jobs" {
  name = "job-queue"
}

# --- Route53 ---
resource "aws_route53_zone" "main" {
  name = "example.com"
}

# --- Lambda (different config) ---
resource "aws_lambda_function" "api" {
  function_name = "api-handler"
  role          = "arn:aws:lambda:us-east-1:aws:resource-id"
  handler       = "index.handler"
  runtime       = "python3.9"
  filename      = "function.zip"
  memory_size   = 512
}
