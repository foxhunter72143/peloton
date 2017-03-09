.PHONY: all master placement executor install client test cover lint clean hostmgr jobmgr resmgr docker version debs docker-push test-containers

PROJECT_ROOT  = code.uber.internal/infra/peloton

# all .go files that don't exist in hidden directories
ALL_SRC := $(shell find . -name "*.go" | grep -v -e Godeps -e vendor -e go-build \
	-e ".*/\..*" \
	-e ".*/_.*" \
	-e ".*/mocks.*" \
	-e ".*/*.pb.go")
BIN_DIR = bin
FMT_SRC:=$(shell echo "$(ALL_SRC)" | tr ' ' '\n')
ALL_PKGS = $(shell go list $(sort $(dir $(ALL_SRC))) | grep -v vendor | grep -v mesos-go)
PBGEN_DIR = pbgen/src
PROTOC = protoc
PACKAGE_VERSION=`git describe --always --tags`
DOCKER_IMAGE ?= uber/peloton
DC ?= all
PROTOC_FLAGS = --proto_path=protobuf --go_out=$(PBGEN_DIR)
PBFILES = $(shell find protobuf -name *.proto)
PBGENS = $(PBFILES:%.proto=%.pb.go)
GOCOV = $(go get github.com/axw/gocov/gocov)
GOCOV_XML = $(go get github.com/AlekSi/gocov-xml)
GOLINT = $(go get github.com/golang/lint/golint)
PHAB_COMMENT = .phabricator-comment
PACKAGE_VERSION=`git describe --always --tags`
# See https://golang.org/doc/gdb for details of the flags
GO_FLAGS = -gcflags '-N -l' -ldflags "-X main.version=$(PACKAGE_VERSION)"
# TODO: figure out why -pkgdir does not work
GOPATH := ${GOPATH}:${GOPATH}/src/${PROJECT_ROOT}/pbgen
.PRECIOUS: $(PBGENS)


all: $(PBGENS) master placement executor client hostmgr resmgr jobmgr

master:
	@mkdir -p $(BIN_DIR)
	go build $(GO_FLAGS) -o ./$(BIN_DIR)/peloton-master master/main/*.go

jobmgr:
	go build $(GO_FLAGS) -o ./$(BIN_DIR)/peloton-jobmgr jobmgr/main/*.go

hostmgr:
	go build $(GO_FLAGS) -o ./$(BIN_DIR)/peloton-hostmgr hostmgr/main/*.go

placement:
	go build $(GO_FLAGS) -o ./$(BIN_DIR)/peloton-placement placement/main/*.go

resmgr:
	go build $(GO_FLAGS) -o ./$(BIN_DIR)/peloton-resmgr resmgr/main/*.go

executor:
	go build $(GO_FLAGS) -o ./$(BIN_DIR)/peloton-executor executor/*.go

install:
	glide --version || go get github.com/Masterminds/glide
	glide install

client:
	go build $(GO_FLAGS) -o ./$(BIN_DIR)/peloton client/cli/*.go

cover:
	./scripts/cover.sh $(shell go list $(PACKAGES))
	go tool cover -html=cover.out -o cover.html

clean:
	rm -rf pbgen
	rm -rf $(BIN_DIR)

format fmt: ## Runs "gofmt $(FMT_FLAGS) -w" to reformat all Go files
	@gofmt -s -w $(FMT_SRC)

# launch the test containers to run integration tests and so-on
test-containers:
	bash docker/run_test_mysql.sh
	bash docker/run_test_cassandra.sh

test: $(GOCOV) $(PBGENS) test-containers
	gocov test $(ALL_PKGS) | gocov report

# launch peloton with PELOTON={app,master}, default to none
pcluster:
# installaltion of docker-py is required, see "bootstrap.sh" or ""tools/pcluster/README.md" for more info
ifndef PELOTON
	@./tools/pcluster/pcluster.py setup
else
ifeq ($(PELOTON),master)
	@./tools/pcluster/pcluster.py setup -m
else ifeq ($(PELOTON),app)
	@./tools/pcluster/pcluster.py setup -a
else
	@echo "Unknown Peloton mode: "$(PELOTON)
endif
endif

pcluster-teardown:
	@./tools/pcluster/pcluster.py teardown

devtools:
	@echo "Installing tools"
	go get github.com/axw/gocov/gocov
	go get github.com/AlekSi/gocov-xml
	go get github.com/matm/gocov-html
	go get github.com/golang/lint/golint

version:
	@echo $(PACKAGE_VERSION)

project-name:
	@echo $(PROJECT_ROOT)

debs:
	@./tools/packaging/build-pkg.sh

# override the built image with IMAGE=
docker:
ifndef IMAGE
	@./tools/packaging/build-docker.sh $(DOCKER_IMAGE):$(PACKAGE_VERSION)
else
	@./tools/packaging/build-docker.sh $(IMAGE)
endif

# override the image to push with IMAGE=
docker-push:
ifndef IMAGE
	@./tools/packaging/docker-push.sh $(DOCKER_IMAGE):$(PACKAGE_VERSION)
else
	@./tools/packaging/docker-push.sh $(IMAGE)
endif

%.pb.go: %.proto
	@mkdir -p $(PBGEN_DIR)
	${PROTOC} ${PROTOC_FLAGS} $<

# Jenkins related tasks

LINT_SKIP_ERRORF=grep -v -e "not a string in call to Errorf"
FILTER_LINT := $(if $(LINT_EXCLUDES), grep -v $(foreach file, $(LINT_EXCLUDES),-e $(file)),cat) | $(LINT_SKIP_ERRORF)
# Runs all Go code through "go vet", "golint", and ensures files are formatted using "gofmt"
lint: devtools
	@echo "Running lint"
	@# Skip the last line of the vet output if it contains "exit status"
	@go vet $(ALL_PKGS) 2>&1 | sed '/exit status 1/d' | $(FILTER_LINT) > vet.log || true
	@if [ -s "vet.log" ] ; then \
	    (echo "Go Vet Failures" | cat - vet.log | tee -a $(PHAB_COMMENT) && false) \
	fi;

	@cat /dev/null > vet.log
	@gofmt -e -s -l $(FMT_SRC) | $(FILTER_LINT) > vet.log || true
	@if [ -s "vet.log" ] ; then \
	    (echo "Go Fmt Failures, run 'make fmt'" | cat - vet.log | tee -a $(PHAB_COMMENT) && false) \
	fi;

jenkins: devtools lint $(PBGENS)
	@chmod -R 777 $(dir $(PBGEN_DIR))
	gocov test -v -race $(ALL_PKGS) > coverage.json | sed 's|filename=".*$(PROJECT_ROOT)/|filename="|'
	gocov-xml < coverage.json > coverage.xml
