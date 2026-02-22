# Contributing to Concord

Thank you for your interest in contributing to Concord! This document provides guidelines and best practices.

## Table of Contents
- [Code of Conduct](#code-of-conduct)
- [Getting Started](#getting-started)
- [Development Workflow](#development-workflow)
- [Coding Standards](#coding-standards)
- [Commit Messages](#commit-messages)
- [Pull Request Process](#pull-request-process)
- [Security](#security)

## Code of Conduct

- Be respectful and inclusive
- Assume good intentions
- Provide constructive feedback
- Focus on the code, not the person

## Getting Started

### Prerequisites
- Go 1.24+
- Node.js 20+
- Docker & Docker Compose
- Git

### Setup
```bash
# Clone the repository
git clone https://github.com/markdrogersjr/Concord.git
cd Concord

# Install Git hooks (prevents committing secrets)
./scripts/install-git-hooks.sh

# Start development environment
cd scripts
./dev-start.sh

# Run tests
cd ../services/control-plane
go test ./...
```

## Development Workflow

### 1. Create a Branch
```bash
git checkout main
git pull origin main
git checkout -b feature/your-feature-name
```

### 2. Make Changes
- Write code
- Add tests
- Update documentation

### 3. Test Locally
```bash
# Backend tests
cd services/control-plane
go test -v ./...

# Frontend build
cd client/desktop
npm run build
```

### 4. Commit
```bash
git add .
git commit -m "feat: add your feature"
```

### 5. Push and Create PR
```bash
git push origin feature/your-feature-name
# Then open a Pull Request on GitHub
```

## Coding Standards

### Go (Backend)
```go
// Use gofmt and golangci-lint
// Follow Effective Go guidelines

// Good example:
func ValidateUsername(username string) error {
    if len(username) < 3 {
        return fmt.Errorf("username too short")
    }
    return nil
}
```

**Go Best Practices:**
- Use meaningful variable names
- Handle all errors explicitly
- Add comments for exported functions
- Keep functions small and focused
- Use context for cancellation

### TypeScript (Frontend)
```typescript
// Use ESLint and Prettier
// Follow Airbnb style guide

// Good example:
interface User {
  id: string;
  username: string;
  email: string;
}

const validateUser = (user: User): boolean => {
  return user.username.length >= 3;
};
```

**TypeScript Best Practices:**
- Use TypeScript strict mode
- Prefer interfaces over types
- Avoid `any` type
- Use functional components with hooks

### Database
- Always use migrations for schema changes
- Never store plaintext passwords
- Use prepared statements (we do via pgx)
- Index foreign keys

## Commit Messages

Format: `<type>: <description>`

**Types:**
- `feat` - New feature
- `fix` - Bug fix
- `refactor` - Code refactoring
- `docs` - Documentation
- `test` - Tests
- `chore` - Maintenance
- `perf` - Performance
- `style` - Code style

**Examples:**
```bash
git commit -m "feat: add username validation"
git commit -m "fix: resolve token expiration bug"
git commit -m "docs: update API documentation"
```

**Multi-line commits:**
```bash
git commit -m "feat: add E2EE key management

- Implemented RSA key pair generation
- Added AES-GCM key wrapping
- Created secure key storage
- Added unit tests

Closes #42"
```

## Pull Request Process

### Before Creating a PR

- [ ] Tests pass locally
- [ ] Code follows style guidelines
- [ ] Documentation updated
- [ ] No merge conflicts with main
- [ ] Commit messages follow format
- [ ] No secrets committed (hooks will catch this)

### Creating a PR

1. **Use the PR template** (auto-fills)
2. **Write a clear description**
   - What does it do?
   - Why is it needed?
   - How was it tested?
3. **Link related issues** (`Closes #123`)
4. **Request review** from a team member
5. **Mark as draft** if work-in-progress

### Review Process

**As an Author:**
- Respond to all comments
- Make requested changes
- Mark conversations as resolved
- Be patient and respectful

**As a Reviewer:**
- Review within 24 hours
- Be constructive, not critical
- Ask questions if unclear
- Approve when satisfied

### Merging

- ✅ **Squash and merge** (keeps history clean)
- ✅ **Delete branch** after merge
- ❌ **Don't merge your own PRs** (unless emergency)

## Security

### Never Commit:
- ❌ Passwords or API keys
- ❌ Private keys (.pem, .key)
- ❌ .env files with secrets
- ❌ Database credentials
- ❌ JWT secrets

### Always Use:
- ✅ Environment variables
- ✅ .env.example templates
- ✅ GitHub Secrets for CI/CD
- ✅ Git hooks (auto-installed)

### Security Checklist:
- [ ] No hardcoded secrets
- [ ] Input validation on all user data
- [ ] SQL injection prevention (use parameterized queries)
- [ ] XSS prevention (sanitize output)
- [ ] CSRF tokens on state-changing requests
- [ ] Rate limiting on sensitive endpoints
- [ ] Authentication on protected routes
- [ ] Authorization checks (is user allowed?)

## Testing

### Backend (Go)
```bash
cd services/control-plane

# Run tests
go test ./...

# With coverage
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out

# Race detector
go test -race ./...
```

### Frontend (Node)
```bash
cd client/desktop

# Run tests (when available)
npm test

# Linting
npm run lint
```

## Documentation

- Update README.md for user-facing changes
- Add comments to complex code
- Document API endpoints in OpenAPI/Swagger
- Create migration guides for breaking changes

## Questions?

- **General questions:** Open a GitHub Discussion
- **Bugs:** Create an issue
- **Security:** Email security@concordvoice.chat
- **Features:** Create a feature request

## License

By contributing, you agree that your contributions will be licensed under the same license as the project (specify license here).

---

Thank you for contributing to Concord! 🚀
