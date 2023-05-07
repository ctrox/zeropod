package wrapper

import (
	"context"

	"github.com/containerd/containerd/pkg/shutdown"
	"github.com/containerd/containerd/runtime/v2/shim"
	taskAPI "github.com/containerd/containerd/runtime/v2/task"
	"github.com/ctrox/zeropod/runc/task"
	types1 "github.com/gogo/protobuf/types"
)

func NewWrapperService(ctx context.Context, publisher shim.Publisher, sd shutdown.Service) (taskAPI.TaskService, error) {
	task, err := task.NewTaskService(ctx, publisher, sd)
	return &wrapper{real: task}, err
}

type wrapper struct {
	real taskAPI.TaskService
}

func (s *wrapper) State(ctx context.Context, req *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	return s.real.State(ctx, req)
}

func (s *wrapper) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	return s.real.Create(ctx, r)
}

func (s *wrapper) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	resp, err := s.real.Start(ctx, r)

	return resp, err
}

func (s *wrapper) Delete(ctx context.Context, req *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	return s.real.Delete(ctx, req)
}

func (s *wrapper) Pids(ctx context.Context, req *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	return s.real.Pids(ctx, req)
}

func (s *wrapper) Pause(ctx context.Context, req *taskAPI.PauseRequest) (*types1.Empty, error) {
	return s.real.Pause(ctx, req)
}

func (s *wrapper) Resume(ctx context.Context, req *taskAPI.ResumeRequest) (*types1.Empty, error) {
	return s.real.Resume(ctx, req)
}

func (s *wrapper) Checkpoint(ctx context.Context, req *taskAPI.CheckpointTaskRequest) (*types1.Empty, error) {
	return s.real.Checkpoint(ctx, req)
}

func (s *wrapper) Kill(ctx context.Context, req *taskAPI.KillRequest) (*types1.Empty, error) {
	return s.real.Kill(ctx, req)
}

func (s *wrapper) Exec(ctx context.Context, req *taskAPI.ExecProcessRequest) (*types1.Empty, error) {
	return s.real.Exec(ctx, req)
}

func (s *wrapper) ResizePty(ctx context.Context, req *taskAPI.ResizePtyRequest) (*types1.Empty, error) {
	return s.real.ResizePty(ctx, req)
}

func (s *wrapper) CloseIO(ctx context.Context, req *taskAPI.CloseIORequest) (*types1.Empty, error) {
	return s.real.CloseIO(ctx, req)
}

func (s *wrapper) Update(ctx context.Context, req *taskAPI.UpdateTaskRequest) (*types1.Empty, error) {
	return s.real.Update(ctx, req)
}

func (s *wrapper) Wait(ctx context.Context, req *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	return s.real.Wait(ctx, req)
}

func (s *wrapper) Stats(ctx context.Context, req *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	return s.real.Stats(ctx, req)
}

func (s *wrapper) Connect(ctx context.Context, req *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	return s.real.Connect(ctx, req)
}

func (s *wrapper) Shutdown(ctx context.Context, req *taskAPI.ShutdownRequest) (*types1.Empty, error) {
	return s.real.Shutdown(ctx, req)
}
