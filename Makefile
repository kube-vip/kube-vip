SHELL := /bin/sh

# The name of the executable (default is current directory name)
TARGET := kube-vip
.DEFAULT_GOAL := $(TARGET)

# These will be provided to the target
VERSION := v1.0.3

BUILD := `git rev-parse HEAD`

# Operating System Default (LINUX)
TARGETOS=linux

# Use linker flags to provide version/build settings to the target
LDFLAGS=-ldflags "-s -w -X=main.Version=$(VERSION) -X=main.Build=$(BUILD) -extldflags -static"
DOCKERTAG ?= $(VERSION)
REPOSITORY ?= docker.io/plndr
GO_VERSION := 1.25.5

.PHONY: all build clean install uninstall simplify check run e2e-tests unit-tests integration-tests unit-tests-docker integration-tests-docker

all: check install

$(TARGET):
	@go build $(LDFLAGS) -o $(TARGET)

build: $(TARGET)
	@true

clean:
	@rm -f $(TARGET)

install:
	@echo Building and Installing project
	@go install $(LDFLAGS)

uninstall: clean
	@rm -f $$(which ${TARGET})

demo:
	@cd demo
	@docker buildx build  --platform linux/amd64,linux/arm64,linux/arm/v7,linux/ppc64le,linux/s390x --push -t $(REPOSITORY)/$(TARGET):$(DOCKERTAG) .
	@echo New Multi Architecture Docker image created
	@cd ..

## Remote (push of images)
# This build a local docker image (x86 only) for quick testing

dockerx86Dev:
	@-rm ./kube-vip
	@docker buildx build  --platform linux/amd64 --push -t $(REPOSITORY)/$(TARGET):dev .
	@echo New single x86 Architecture Docker image created

dockerx86Iptables:
	@-rm ./kube-vip
	@docker buildx build  --platform linux/amd64 -f ./Dockerfile_iptables --push -t $(REPOSITORY)/$(TARGET):dev .
	@echo New single x86 Architecture Docker image created

dockerx86IptablesLocal:
	@-rm ./kube-vip
	@docker buildx build  --platform linux/amd64 -f ./Dockerfile_iptables -t $(REPOSITORY)/$(TARGET):$(DOCKERTAG) .
	@echo New single x86 Architecture Docker image created

dockerx86:
	@-rm ./kube-vip
	@docker buildx build  --platform linux/amd64 --push -t $(REPOSITORY)/$(TARGET):$(DOCKERTAG) .
	@echo New single x86 Architecture Docker image created

docker:
	@-rm ./kube-vip
	@docker buildx build  --platform linux/amd64,linux/arm64,linux/arm/v7,linux/ppc64le,linux/s390x --push -t $(REPOSITORY)/$(TARGET):$(DOCKERTAG) .
	@echo New Multi Architecture Docker image created

## Local (docker load of images)
# This will build a local docker image (x86 only), use make dockerLocal for all architectures
dockerx86Local:
	@-rm ./kube-vip
	@docker buildx build  --platform linux/amd64 --load -t $(REPOSITORY)/$(TARGET):$(DOCKERTAG) .
	@echo New Multi Architecture Docker image created

dockerx86Action:
	@-rm ./kube-vip
	@docker buildx build  --platform linux/amd64 --load -t $(REPOSITORY)/$(TARGET):action .
	@echo New Multi Architecture Docker image created

dockerx86ActionIPTables:
	@-rm ./kube-vip
	@docker buildx build  --platform linux/amd64 -f ./Dockerfile_iptables --load -t $(REPOSITORY)/$(TARGET):action .
	@echo New Multi Architecture Docker image created

dockerLocal:
	@-rm ./kube-vip
	@docker buildx build  --platform linux/amd64,linux/arm64,linux/arm/v7,linux/ppc64le,linux/s390x --load -t $(REPOSITORY)/$(TARGET):$(DOCKERTAG) .
	@echo New Multi Architecture Docker image created

simplify:
	@gofmt -s -l -w *.go pkg cmd

check:
	go mod tidy
	test -z "$(git status --porcelain)"
	test -z $(shell gofmt -l *.go pkg cmd) || echo "[WARN] Fix formatting issues with 'make simplify'"
	golangci-lint run
	go vet ./...

run: install
	@$(TARGET)

manifests:
	@make build
	@mkdir -p ./docs/manifests/$(VERSION)/
	@./kube-vip manifest pod --interface eth0 --vip 192.168.0.1 --image "$(REPOSITORY)/$(TARGET):$(DOCKERTAG)" --arp --leaderElection --controlplane --services > ./docs/manifests/$(VERSION)/kube-vip-arp.yaml
	@./kube-vip manifest pod --interface eth0 --vip 192.168.0.1 --image "$(REPOSITORY)/$(TARGET):$(DOCKERTAG)" --arp --leaderElection --controlplane --services --enableLoadBalancer > ./docs/manifests/$(VERSION)/kube-vip-arp-lb.yaml
	@./kube-vip manifest pod --interface eth0 --vip 192.168.0.1 --image "$(REPOSITORY)/$(TARGET):$(DOCKERTAG)" --bgp --controlplane --services > ./docs/manifests/$(VERSION)/kube-vip-bgp.yaml
	@./kube-vip manifest daemonset --interface eth0 --vip 192.168.0.1 --image "$(REPOSITORY)/$(TARGET):$(DOCKERTAG)" --arp --leaderElection --controlplane --services --inCluster > ./docs/manifests/$(VERSION)/kube-vip-arp-ds.yaml
	@./kube-vip manifest daemonset --interface eth0 --vip 192.168.0.1 --image "$(REPOSITORY)/$(TARGET):$(DOCKERTAG)" --arp --leaderElection --controlplane --services --inCluster --enableLoadBalancer > ./docs/manifests/$(VERSION)/kube-vip-arp-ds-lb.yaml
	@./kube-vip manifest daemonset --interface eth0 --vip 192.168.0.1 --image "$(REPOSITORY)/$(TARGET):$(DOCKERTAG)" --bgp --leaderElection --controlplane --services --inCluster > ./docs/manifests/$(VERSION)/kube-vip-bgp-ds.yaml
	@./kube-vip manifest daemonset --interface eth0 --vip 192.168.0.1 --image "$(REPOSITORY)/$(TARGET):$(DOCKERTAG)" --bgp --leaderElection --controlplane --services --inCluster > ./docs/manifests/$(VERSION)/kube-vip-bgp-em-ds.yaml
	@-rm ./kube-vip

manifest-test: 
	docker run $(REPOSITORY)/$(TARGET):$(DOCKERTAG) manifest pod --interface eth0 --vip 192.168.0.1 --image "$(REPOSITORY)/$(TARGET):$(DOCKERTAG)" --arp --leaderElection --controlplane --services
	docker run $(REPOSITORY)/$(TARGET):$(DOCKERTAG) manifest pod --interface eth0 --vip 192.168.0.1 --image "$(REPOSITORY)/$(TARGET):$(DOCKERTAG)" --arp --leaderElection --controlplane --services --enableLoadBalancer
	docker run $(REPOSITORY)/$(TARGET):$(DOCKERTAG) manifest pod --interface eth0 --vip 192.168.0.1 --image "$(REPOSITORY)/$(TARGET):$(DOCKERTAG)" --bgp --controlplane --services
	docker run $(REPOSITORY)/$(TARGET):$(DOCKERTAG) manifest daemonset --interface eth0 --vip 192.168.0.1 --image "$(REPOSITORY)/$(TARGET):$(DOCKERTAG)" --arp --leaderElection --controlplane --services --inCluster
	docker run $(REPOSITORY)/$(TARGET):$(DOCKERTAG) manifest daemonset --interface eth0 --vip 192.168.0.1 --image "$(REPOSITORY)/$(TARGET):$(DOCKERTAG)" --arp --leaderElection --controlplane --services --inCluster --enableLoadBalancer
	docker run $(REPOSITORY)/$(TARGET):$(DOCKERTAG) manifest daemonset --interface eth0 --vip 192.168.0.1 --image "$(REPOSITORY)/$(TARGET):$(DOCKERTAG)" --bgp --leaderElection --controlplane --services --inCluster

unit-tests:
	go test -race ./...

unit-tests-docker:
	docker run --rm -it -w /kube-vip -v $$(pwd):/kube-vip golang:$(GO_VERSION) make unit-tests

integration-tests:
	go test -tags=integration,e2e -v ./pkg/etcd

integration-tests-docker:
	docker run --rm -it -w /kube-vip -v $$(pwd):/kube-vip golang:$(GO_VERSION) make integration-tests

e2e-tests:
	docker pull ghcr.io/traefik/whoami:v1.11
	GOMAXPROCS=4 E2E_IMAGE_PATH=$(REPOSITORY)/$(TARGET):$(DOCKERTAG) go run github.com/onsi/ginkgo/v2/ginkgo --tags=e2e -v -p ./testing/e2e ./testing/e2e/etcd

e2e-tests129-arp:
	docker pull ghcr.io/traefik/whoami:v1.11
	GOMAXPROCS=4 TEST_MODE=arp V129=true K8S_IMAGE_PATH=kindest/node:v1.29.0 E2E_IMAGE_PATH=$(REPOSITORY)/$(TARGET):$(DOCKERTAG) go run github.com/onsi/ginkgo/v2/ginkgo --tags=e2e -v -p ./testing/e2e

e2e-tests129-rt:
	docker pull ghcr.io/traefik/whoami:v1.11
	GOMAXPROCS=4 TEST_MODE=rt V129=true K8S_IMAGE_PATH=kindest/node:v1.29.0 E2E_IMAGE_PATH=$(REPOSITORY)/$(TARGET):$(DOCKERTAG) go run github.com/onsi/ginkgo/v2/ginkgo --tags=e2e -v -p ./testing/e2e

e2e-tests129-bgp:
	docker pull ghcr.io/traefik/whoami:v1.11
	GOMAXPROCS=4 TEST_MODE=bgp V129=true K8S_IMAGE_PATH=kindest/node:v1.29.0 E2E_IMAGE_PATH=$(REPOSITORY)/$(TARGET):$(DOCKERTAG) go run github.com/onsi/ginkgo/v2/ginkgo --tags=e2e -v -p ./testing/e2e

e2e-tests129: e2e-tests129-arp e2e-tests129-rt e2e-tests129-bgp

service-tests: 
	$(MAKE) -C testing/e2e/e2e dockerLocal
	E2E_IMAGE_PATH=$(REPOSITORY)/$(TARGET):$(DOCKERTAG) go run ./testing/services -Services -simple -deployments -leaderActive -leaderFailover -localDeploy -egress -egressIPv6 -dualStack

trivy: dockerx86ActionIPTables
	docker run -v /var/run/docker.sock:/var/run/docker.sock aquasec/trivy:0.47.0 \
		image  \
		--format table \
		--exit-code  1 \
		--ignore-unfixed \
		--vuln-type  'os,library' \
		--severity  'CRITICAL,HIGH'  \
		$(REPOSITORY)/$(TARGET):action

kind-quick:
	echo "Standing up your cluster"
	kind create cluster --config ./testing/kind/kind.yaml --name kube-vip
	kubectl apply -f https://kube-vip.io/manifests/rbac.yaml
	kubectl create configmap --namespace kube-system kubevip --from-literal range-global=172.18.100.10-172.18.100.30
	kubectl apply -f https://raw.githubusercontent.com/kube-vip/kube-vip-cloud-provider/main/manifest/kube-vip-cloud-controller.yaml
	kind load docker-image --name kube-vip $(REPOSITORY)/$(TARGET):$(DOCKERTAG)
	docker run --network host --rm $(REPOSITORY)/$(TARGET):$(DOCKERTAG) manifest daemonset --services --inCluster --image "$(REPOSITORY)/$(TARGET):$(DOCKERTAG)" --arp --servicesElection --interface  eth0 | kubectl apply -f -

kind-reload:
	kind load docker-image $(REPOSITORY)/$(TARGET):$(DOCKERTAG) --name services
	kubectl rollout restart -n kube-system daemonset/kube-vip-ds

get-gobgp:
	mkdir -p bin
	wget -nc --directory-prefix=bin https://github.com/osrg/gobgp/releases/download/v3.37.0/gobgp_3.37.0_linux_amd64.tar.gz
	tar -xvzf bin/gobgp_3.37.0_linux_amd64.tar.gz -C bin

