package v1

import "path/filepath"

const (
	runPath              = "/run/zeropod/"
	SocketPath           = runPath + "node.sock"
	ImagesPath           = runPath + "i/"
	SnapshotSuffix       = "snapshot"
	WorkDirSuffix        = "work"
	MigrateAnnotationKey = "zeropod.ctrox.dev/migrate"
	NodeNameEnvKey       = "NODE_NAME"
	PodIPEnvKey          = "POD_IP"
)

func ImagePath(id string) string {
	return filepath.Join(ImagesPath, id)
}

func WorkDirPath(id string) string {
	return filepath.Join(ImagesPath, id, WorkDirSuffix)
}

func SnapshotPath(id string) string {
	return filepath.Join(ImagesPath, id, SnapshotSuffix)
}

func LazyPagesSocket(id string) string {
	return filepath.Join(runPath, id+".sock")
}
