package v1

import "path/filepath"

const (
	runPath                  = "/run/zeropod/"
	SocketPath               = runPath + "node.sock"
	SnapshotSuffix           = "snapshot"
	UpperSuffix              = "upper"
	WorkDirSuffix            = "work"
	MigrateAnnotationKey     = "zeropod.ctrox.dev/migrate"
	LiveMigrateAnnotationKey = "zeropod.ctrox.dev/live-migrate"
	NodeNameEnvKey           = "NODE_NAME"
	PodIPEnvKey              = "POD_IP"
	preDumpDirName           = "pre-dump"
)

var imageBasePath = "/var/lib/zeropod/"

func SetImageBasePath(path string) {
	imageBasePath = path
}

func ImagePath(id string) string {
	return filepath.Join(imageBasePath, "i", id)
}

func WorkDirPath(id string) string {
	return filepath.Join(ImagePath(id), WorkDirSuffix)
}

func SnapshotPath(id string) string {
	return filepath.Join(ImagePath(id), SnapshotSuffix)
}

func UpperPath(id string) string {
	return filepath.Join(ImagePath(id), SnapshotSuffix, UpperSuffix)
}

func LazyPagesSocket(id string) string {
	return filepath.Join(runPath, id+".sock")
}

func PreDumpDir(id string) string {
	return filepath.Join(SnapshotPath(id), preDumpDirName)
}

func RelativePreDumpDir() string {
	return filepath.Join("..", SnapshotSuffix, preDumpDirName)
}
