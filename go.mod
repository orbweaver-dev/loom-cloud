module github.com/orbweaver-dev/loom-cloud

go 1.25.0

// Local development: point at the in-progress loom checkout
// next door. Production / release builds replace this with the
// pinned tagged version once orbweaver-dev/loom is published
// publicly (sum.golang.org can't yet verify the private repo).
replace github.com/orbweaver-dev/loom => ../loom

require (
	github.com/orbweaver-dev/loom v0.7.0
	github.com/stretchr/testify v1.10.0
	golang.org/x/crypto v0.51.0
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jmoiron/sqlx v1.4.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
