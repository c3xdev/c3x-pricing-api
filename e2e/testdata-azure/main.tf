provider "azurerm" {
  skip_provider_registration = true
  features {}
}

# --- Linux VM ---
resource "azurerm_linux_virtual_machine" "web" {
  name                = "web-vm"
  resource_group_name = "rg-test"
  location            = "eastus"
  size                = "Standard_D2_v3"
  admin_username      = "adminuser"

  admin_ssh_key {
    username   = "adminuser"
    public_key = "ssh-rsa AAAAB3NzaC1yc2EAAA"
  }

  network_interface_ids = ["/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-test/providers/Microsoft.Network/networkInterfaces/nic1"]

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = "Premium_LRS"
    disk_size_gb         = 64
  }

  source_image_reference {
    publisher = "Canonical"
    offer     = "UbuntuServer"
    sku       = "18.04-LTS"
    version   = "latest"
  }
}

# --- Managed Disk ---
resource "azurerm_managed_disk" "data" {
  name                 = "data-disk"
  location             = "eastus"
  resource_group_name  = "rg-test"
  storage_account_type = "Premium_LRS"
  disk_size_gb         = 256
  create_option        = "Empty"
}

# --- App Service Plan ---
resource "azurerm_app_service_plan" "main" {
  name                = "app-plan"
  location            = "eastus"
  resource_group_name = "rg-test"
  kind                = "Linux"
  reserved            = true

  sku {
    tier = "Standard"
    size = "S1"
  }
}

# --- DNS Zone ---
resource "azurerm_dns_zone" "main" {
  name                = "example.com"
  resource_group_name = "rg-test"
}

# --- Load Balancer ---
resource "azurerm_lb" "web" {
  name                = "web-lb"
  location            = "eastus"
  resource_group_name = "rg-test"
  sku                 = "Standard"

  frontend_ip_configuration {
    name                 = "frontend"
    public_ip_address_id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-test/providers/Microsoft.Network/publicIPAddresses/pip1"
  }
}

# --- Container Instance ---
resource "azurerm_container_group" "app" {
  name                = "app-container"
  location            = "eastus"
  resource_group_name = "rg-test"
  os_type             = "Linux"

  container {
    name   = "app"
    image  = "nginx:latest"
    cpu    = 2
    memory = 4

    ports {
      port     = 80
      protocol = "TCP"
    }
  }
}

# --- VPN Gateway ---
resource "azurerm_virtual_network_gateway" "main" {
  name                = "vpn-gw"
  location            = "eastus"
  resource_group_name = "rg-test"
  type                = "Vpn"
  vpn_type            = "RouteBased"
  sku                 = "VpnGw1"

  ip_configuration {
    public_ip_address_id          = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-test/providers/Microsoft.Network/publicIPAddresses/pip2"
    private_ip_address_allocation = "Dynamic"
    subnet_id                     = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-test/providers/Microsoft.Network/virtualNetworks/vnet1/subnets/GatewaySubnet"
  }
}
