package task

import (
	"context"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/ttrpc"
)

const (
	taskServiceV2 = "containerd.task.v2.Task"
	taskServiceV3 = "containerd.task.v3.Task"
)

// registerTaskService registers a task service with the provided name. This is
// a bit of a hack to register a v3 task service as a v2 service. Since the API
// has not changed at all this works just fine.
func registerTaskService(name string, srv *ttrpc.Server, svc taskAPI.TTRPCTaskService) {
	srv.RegisterService(name, &ttrpc.ServiceDesc{
		Methods: map[string]ttrpc.Method{
			"State": func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
				var req taskAPI.StateRequest
				if err := unmarshal(&req); err != nil {
					return nil, err
				}
				return svc.State(ctx, &req)
			},
			"Create": func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
				var req taskAPI.CreateTaskRequest
				if err := unmarshal(&req); err != nil {
					return nil, err
				}
				return svc.Create(ctx, &req)
			},
			"Start": func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
				var req taskAPI.StartRequest
				if err := unmarshal(&req); err != nil {
					return nil, err
				}
				return svc.Start(ctx, &req)
			},
			"Delete": func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
				var req taskAPI.DeleteRequest
				if err := unmarshal(&req); err != nil {
					return nil, err
				}
				return svc.Delete(ctx, &req)
			},
			"Pids": func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
				var req taskAPI.PidsRequest
				if err := unmarshal(&req); err != nil {
					return nil, err
				}
				return svc.Pids(ctx, &req)
			},
			"Pause": func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
				var req taskAPI.PauseRequest
				if err := unmarshal(&req); err != nil {
					return nil, err
				}
				return svc.Pause(ctx, &req)
			},
			"Resume": func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
				var req taskAPI.ResumeRequest
				if err := unmarshal(&req); err != nil {
					return nil, err
				}
				return svc.Resume(ctx, &req)
			},
			"Checkpoint": func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
				var req taskAPI.CheckpointTaskRequest
				if err := unmarshal(&req); err != nil {
					return nil, err
				}
				return svc.Checkpoint(ctx, &req)
			},
			"Kill": func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
				var req taskAPI.KillRequest
				if err := unmarshal(&req); err != nil {
					return nil, err
				}
				return svc.Kill(ctx, &req)
			},
			"Exec": func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
				var req taskAPI.ExecProcessRequest
				if err := unmarshal(&req); err != nil {
					return nil, err
				}
				return svc.Exec(ctx, &req)
			},
			"ResizePty": func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
				var req taskAPI.ResizePtyRequest
				if err := unmarshal(&req); err != nil {
					return nil, err
				}
				return svc.ResizePty(ctx, &req)
			},
			"CloseIO": func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
				var req taskAPI.CloseIORequest
				if err := unmarshal(&req); err != nil {
					return nil, err
				}
				return svc.CloseIO(ctx, &req)
			},
			"Update": func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
				var req taskAPI.UpdateTaskRequest
				if err := unmarshal(&req); err != nil {
					return nil, err
				}
				return svc.Update(ctx, &req)
			},
			"Wait": func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
				var req taskAPI.WaitRequest
				if err := unmarshal(&req); err != nil {
					return nil, err
				}
				return svc.Wait(ctx, &req)
			},
			"Stats": func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
				var req taskAPI.StatsRequest
				if err := unmarshal(&req); err != nil {
					return nil, err
				}
				return svc.Stats(ctx, &req)
			},
			"Connect": func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
				var req taskAPI.ConnectRequest
				if err := unmarshal(&req); err != nil {
					return nil, err
				}
				return svc.Connect(ctx, &req)
			},
			"Shutdown": func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
				var req taskAPI.ShutdownRequest
				if err := unmarshal(&req); err != nil {
					return nil, err
				}
				return svc.Shutdown(ctx, &req)
			},
		},
	})
}
