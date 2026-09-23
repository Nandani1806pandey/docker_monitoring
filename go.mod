module github.com/nandani/docker-monitor

go 1.22.2

require github.com/google/uuid v1.6.0

require github.com/gorilla/websocket v1.5.3

require (
	github.com/lib/pq v1.10.9
	github.com/mattn/go-sqlite3 v1.14.22
	golang.org/x/crypto v0.28.0
)

require golang.org/x/sys v0.26.0 // indirect

replace golang.org/x/crypto => github.com/golang/crypto v0.28.0

replace golang.org/x/sys => github.com/golang/sys v0.26.0
