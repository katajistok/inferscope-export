BINARY     := axon
BUILD_DIR  := ./bin
CMD        := ./cmd/axon
INSTALL    := /usr/local/bin/$(BINARY)
SERVICE    := deploy/systemd/axon.service
SERVICE_DST:= /etc/systemd/system/axon.service

.PHONY: all build install uninstall purge-all systemd-install systemd-start systemd-stop \
        systemd-status docker-build docker-run local-up local-down local-purge local-logs fmt vet test clean

all: build

build:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 go build -ldflags="-s -w" -o $(BUILD_DIR)/$(BINARY) $(CMD)
	@echo "Built: $(BUILD_DIR)/$(BINARY)"

# Install binary + systemd unit and start service
install: build
	install -m 0755 $(BUILD_DIR)/$(BINARY) $(INSTALL)
	@echo "Installed: $(INSTALL)"

systemd-install: install
	install -m 0644 $(SERVICE) $(SERVICE_DST)
	systemctl daemon-reload
	@echo "Service installed: $(SERVICE_DST)"
	@echo "Run: make systemd-start"

systemd-start:
	systemctl enable --now axon
	@echo "Service started. Logs: journalctl -u axon -f"

systemd-stop:
	systemctl disable --now axon

systemd-status:
	systemctl status axon

systemd-logs:
	journalctl -u axon -f

# Remove axon service + binary (keeps Prometheus/Grafana data)
uninstall: systemd-stop
	rm -f $(INSTALL) $(SERVICE_DST)
	systemctl daemon-reload
	@echo "Axon uninstalled."

# Remove Prometheus + Grafana containers AND their stored data volumes
local-purge:
	docker compose -f deploy/local/docker-compose.yml down -v --remove-orphans
	@echo "Prometheus and Grafana containers and data volumes removed."

# Remove everything: axon + monitoring stack + data
purge-all: uninstall local-purge
	@echo "Inferscope fully removed."

# Docker
docker-build:
	docker build -f deploy/docker/Dockerfile -t axon:latest .

docker-run:
	docker compose -f deploy/docker/docker-compose.yml up

# Local monitoring stack (Prometheus + Grafana)
local-up:
	docker compose -f deploy/local/docker-compose.yml up -d
	@echo "Grafana  → http://localhost:3000  (admin / admin)"
	@echo "Prometheus → http://localhost:9090"

local-down:
	docker compose -f deploy/local/docker-compose.yml down

local-logs:
	docker compose -f deploy/local/docker-compose.yml logs -f

fmt:
	gofmt -w .

vet:
	go vet ./...

test:
	go test ./... -v

clean:
	rm -rf $(BUILD_DIR)
