# Contributing

Thank you for your interest in contributing to the C3X Pricing API.

## Getting Started

1. Fork the repository
2. Clone your fork locally
3. Set up the development environment:
   ```bash
   cp .env.example .env  # configure your environment
   docker compose up -d  # start PostgreSQL
   make build
   make test
   ```

## Development Workflow

1. Create a feature branch from `main`
2. Make your changes
3. Run checks before submitting:
   ```bash
   make fmt
   make vet
   make test
   ```
4. Submit a pull request

## Code Standards

- Run `gofmt` on all Go files
- All tests must pass (`make test`)
- `go vet` must report no issues
- Add tests for new functionality
- Keep commits focused and well-described

## Pull Request Guidelines

- Keep PRs focused on a single change
- Include a clear description of what and why
- Reference any related issues
- Ensure CI passes before requesting review
