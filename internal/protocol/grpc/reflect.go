package grpc

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection/grpc_reflection_v1"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// resolveMessages resolves the input and output message descriptors for one
// method through server reflection and returns dynamic messages ready to send
// and receive. No generated Go stubs are required.
func resolveMessages(ctx context.Context, conn *grpc.ClientConn, service, method string) (*dynamicpb.Message, *dynamicpb.Message, error) {
	resolver, err := newReflectionResolver(ctx, conn)
	if err != nil {
		return nil, nil, err
	}
	defer resolver.close()

	if err := resolver.fileContainingSymbol(ctx, service); err != nil {
		return nil, nil, err
	}
	if err := resolver.fillDependencies(ctx); err != nil {
		return nil, nil, err
	}

	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{File: resolver.fileList()})
	if err != nil {
		return nil, nil, fmt.Errorf("link reflected descriptors: %w", err)
	}

	descriptor, err := files.FindDescriptorByName(protoreflect.FullName(service))
	if err != nil {
		return nil, nil, fmt.Errorf("service %q not found: %w", service, err)
	}
	serviceDescriptor, ok := descriptor.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, nil, fmt.Errorf("%q is not a service", service)
	}
	methodDescriptor := serviceDescriptor.Methods().ByName(protoreflect.Name(method))
	if methodDescriptor == nil {
		return nil, nil, fmt.Errorf("service %q has no method %q", service, method)
	}

	return dynamicpb.NewMessage(methodDescriptor.Input()),
		dynamicpb.NewMessage(methodDescriptor.Output()), nil
}

// reflectionResolver owns one bidirectional reflection stream and the file
// descriptors gathered from it.
type reflectionResolver struct {
	stream grpc_reflection_v1.ServerReflection_ServerReflectionInfoClient
	files  map[string]*descriptorpb.FileDescriptorProto
}

// newReflectionResolver opens the reflection stream against a connection.
func newReflectionResolver(ctx context.Context, conn *grpc.ClientConn) (*reflectionResolver, error) {
	client := grpc_reflection_v1.NewServerReflectionClient(conn)
	stream, err := client.ServerReflectionInfo(ctx)
	if err != nil {
		return nil, err
	}
	return &reflectionResolver{
		stream: stream,
		files:  make(map[string]*descriptorpb.FileDescriptorProto),
	}, nil
}

// close half-closes the reflection stream. The stream itself is torn down with
// the connection.
func (r *reflectionResolver) close() {
	_ = r.stream.CloseSend()
}

// fileContainingSymbol requests the file that defines a symbol and records it
// together with any files the server bundled with it.
func (r *reflectionResolver) fileContainingSymbol(ctx context.Context, symbol string) error {
	response, err := r.roundTrip(ctx, &grpc_reflection_v1.ServerReflectionRequest{
		MessageRequest: &grpc_reflection_v1.ServerReflectionRequest_FileContainingSymbol{
			FileContainingSymbol: symbol,
		},
	})
	if err != nil {
		return err
	}
	descriptorResponse := response.GetFileDescriptorResponse()
	if descriptorResponse == nil {
		return fmt.Errorf("reflection returned no file descriptor for %q", symbol)
	}
	return r.record(descriptorResponse.GetFileDescriptorProto())
}

// fileByFilename requests one file descriptor by path.
func (r *reflectionResolver) fileByFilename(ctx context.Context, name string) error {
	response, err := r.roundTrip(ctx, &grpc_reflection_v1.ServerReflectionRequest{
		MessageRequest: &grpc_reflection_v1.ServerReflectionRequest_FileByFilename{
			FileByFilename: name,
		},
	})
	if err != nil {
		return err
	}
	descriptorResponse := response.GetFileDescriptorResponse()
	if descriptorResponse == nil {
		return fmt.Errorf("reflection returned no file descriptor for %q", name)
	}
	return r.record(descriptorResponse.GetFileDescriptorProto())
}

// fillDependencies fetches every transitive dependency the reflected files
// reference but did not bundle, so the set links cleanly. Files already known
// to this process, such as the well-known types, are reused rather than
// requested.
func (r *reflectionResolver) fillDependencies(ctx context.Context) error {
	for {
		var missing []string
		seen := make(map[string]bool)
		for _, file := range r.files {
			for _, dependency := range file.GetDependency() {
				if _, ok := r.files[dependency]; ok || seen[dependency] {
					continue
				}
				seen[dependency] = true
				missing = append(missing, dependency)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		for _, name := range missing {
			if global, err := protoregistry.GlobalFiles.FindFileByPath(name); err == nil {
				r.files[name] = protodesc.ToFileDescriptorProto(global)
				continue
			}
			if err := r.fileByFilename(ctx, name); err != nil {
				return fmt.Errorf("fetch dependency %q: %w", name, err)
			}
		}
	}
}

// roundTrip sends one reflection request and reads its response, translating an
// in-band error response into a Go error.
func (r *reflectionResolver) roundTrip(ctx context.Context, request *grpc_reflection_v1.ServerReflectionRequest) (*grpc_reflection_v1.ServerReflectionResponse, error) {
	if err := r.stream.Send(request); err != nil {
		return nil, err
	}
	response, err := r.stream.Recv()
	if err != nil {
		return nil, err
	}
	if errorResponse := response.GetErrorResponse(); errorResponse != nil {
		return nil, fmt.Errorf("reflection error %d: %s", errorResponse.GetErrorCode(), errorResponse.GetErrorMessage())
	}
	return response, nil
}

// record stores the file descriptors from one reflection response, keyed by
// file name so duplicates collapse.
func (r *reflectionResolver) record(raw [][]byte) error {
	if len(raw) == 0 {
		return fmt.Errorf("reflection returned an empty file descriptor set")
	}
	for _, encoded := range raw {
		file := &descriptorpb.FileDescriptorProto{}
		if err := proto.Unmarshal(encoded, file); err != nil {
			return fmt.Errorf("decode reflected file descriptor: %w", err)
		}
		if name := file.GetName(); name != "" {
			r.files[name] = file
		}
	}
	return nil
}

// fileList returns the gathered descriptors as a slice.
func (r *reflectionResolver) fileList() []*descriptorpb.FileDescriptorProto {
	list := make([]*descriptorpb.FileDescriptorProto, 0, len(r.files))
	for _, file := range r.files {
		list = append(list, file)
	}
	return list
}
