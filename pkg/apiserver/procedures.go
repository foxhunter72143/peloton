// Copyright (c) 2019 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package apiserver

import (
	"fmt"
	"reflect"

	pbv0hostsvc "github.com/uber/peloton/.gen/peloton/api/v0/host/svc"
	pbv0resmgr "github.com/uber/peloton/.gen/peloton/api/v0/respool"
	pbprivateeventstreamsvc "github.com/uber/peloton/.gen/peloton/private/eventstream/v1alpha/eventstreamsvc"
	pbprivatehostsvc "github.com/uber/peloton/.gen/peloton/private/hostmgr/hostsvc"
	pbprivatehostmgrsvc "github.com/uber/peloton/.gen/peloton/private/hostmgr/v1alpha/svc"
	pbprivateresmgrsvc "github.com/uber/peloton/.gen/peloton/private/resmgrsvc"

	"github.com/uber/peloton/pkg/apiserver/forward"
	"github.com/uber/peloton/pkg/common"

	"go.uber.org/yarpc/api/transport"
	"go.uber.org/yarpc/encoding/protobuf"
)

const (
	_procedureNameTemplate = "%s::%s"
)

var (
	// _encodingTypes contains a list of encoding types to build procedures
	// with.
	_encodingTypes []transport.Encoding
)

type rpcService struct {
	// RPC service name from pkg/common/constants.go
	name string
	// Pointer to a (nil) instance of the server; used to fetch RPC API via
	// reflection.
	server interface{}
}

// init constructs required Peloton YARPC clients for the package.
func init() {
	_encodingTypes = []transport.Encoding{
		protobuf.Encoding,
		protobuf.JSONEncoding,
	}
}

// BuildHostManagerProcedures builds forwarding procedures for
// services handled by Host Manager. The outbound must connect to the
// Host Manager leader.
// TODO: refactor to use the BuildYARPCProcedures code machine-generated from
// protobuf files.
func BuildHostManagerProcedures(
	outbound transport.UnaryOutbound,
) []transport.Procedure {
	rpcServers := []rpcService{
		{
			name:   common.RPCPelotonV0HostServiceName,
			server: (*pbv0hostsvc.HostServiceYARPCServer)(nil),
		},
		// TODO: HostService v1 (not implemented in Peloton)
		// Private Event Stream Service doesn't actually accept calls from
		// api server. It inspects RPC caller's service name and only accepts from
		// peloton-jobmgr, peloton-resmgr. This is okay because we do not
		// actually expected these daemons to contact each other through the
		// api server.
		{
			name:   common.RPCPelotonPrivateEventStreamServiceName,
			server: (*pbprivateeventstreamsvc.EventStreamServiceYARPCServer)(nil),
		},
		{
			name:   common.RPCPelotonPrivateHostServiceName,
			server: (*pbprivatehostsvc.InternalHostServiceYARPCServer)(nil),
		},
		{
			name:   common.RPCPelotonPrivateV1AlphaHostManagerServiceName,
			server: (*pbprivatehostmgrsvc.HostManagerServiceYARPCServer)(nil),
		},
	}

	return buildProcedures(rpcServers, common.PelotonHostManager, outbound)
}

// BuildResourceManagerProcedures builds forwarding procedures for
// services handled by Resource Manager. The outbound must connect to the
// Resource Manager leader.
// TODO: refactor to use the BuildYARPCProcedures code machine-generated from
// protobuf files.
func BuildResourceManagerProcedures(
	outbound transport.UnaryOutbound,
) []transport.Procedure {
	rpcServers := []rpcService{
		{
			name:   common.RPCPelotonV0ResourceManagerName,
			server: (*pbv0resmgr.ResourceManagerYARPCServer)(nil),
		},
		// TODO: ResourcePoolService v0 (not implemented in Peloton)
		// TODO: ResourcePoolService v1 (not implemented in Peloton)
		// TODO: TaskQueue Private (not implemented in Peloton)
		{
			name:   common.RPCPelotonPrivateResourceManagerServiceName,
			server: (*pbprivateresmgrsvc.ResourceManagerServiceYARPCServer)(nil),
		},
	}

	return buildProcedures(rpcServers, common.PelotonResourceManager, outbound)
}

func buildProcedures(
	rpcServices []rpcService,
	pelotonApplication string,
	outbound transport.UnaryOutbound,
) []transport.Procedure {
	var procedures []transport.Procedure

	for _, service := range rpcServices {
		ct := reflect.TypeOf(service.server).Elem()
		for i := 0; i < ct.NumMethod(); i++ {
			for _, encoding := range _encodingTypes {
				name := createProcedureName(service.name, ct.Method(i).Name)
				p := buildProcedure(
					name,
					pelotonApplication,
					outbound,
					encoding,
				)
				procedures = append(procedures, p)
			}
		}
	}
	return procedures
}

// buildProcedure builds procedure with given procedure name and outbound.
func buildProcedure(
	procedureName, overrideService string,
	outbound transport.UnaryOutbound,
	encoding transport.Encoding,
) transport.Procedure {
	return transport.Procedure{
		Name: procedureName,
		HandlerSpec: transport.NewUnaryHandlerSpec(
			forward.NewUnaryForward(outbound, overrideService),
		),
		Encoding: encoding,
	}
}

// createProcedureName creates a full procedure name with given service and
// method.
func createProcedureName(service, method string) string {
	return fmt.Sprintf(_procedureNameTemplate, service, method)
}