package replication

import (
	"fmt"
	"github.com/zrepl/zrepl/zfs"
	"time"
)

func (v *FilesystemVersion) RelName() string {
	zv := v.ZFSFilesystemVersion()
	return zv.String()
}

func (v FilesystemVersion_VersionType) ZFSVersionType() zfs.VersionType {
	switch v {
	case FilesystemVersion_Snapshot:
		return zfs.Snapshot
	case FilesystemVersion_Bookmark:
		return zfs.Bookmark
	default:
		panic(fmt.Sprintf("unexpected v.Type %#v", v))
	}
}

func FilesystemVersionFromZFS(fsv zfs.FilesystemVersion) *FilesystemVersion {
	var t FilesystemVersion_VersionType
	switch fsv.Type {
	case zfs.Bookmark:
		t = FilesystemVersion_Bookmark
	case zfs.Snapshot:
		t = FilesystemVersion_Snapshot
	default:
		panic("unknown fsv.Type: " + fsv.Type)
	}
	return &FilesystemVersion{
		Type: t,
		Name: fsv.Name,
		Guid: fsv.Guid,
		CreateTXG: fsv.CreateTXG,
		Creation: fsv.Creation.Format(time.RFC3339),
	}
}

func (v *FilesystemVersion) ZFSFilesystemVersion() *zfs.FilesystemVersion {
	ct := time.Time{}
	if v.Creation != "" {
		var err error
		ct, err = time.Parse(time.RFC3339, v.Creation)
		if err != nil {
			panic(err)
		}
	}
	return &zfs.FilesystemVersion{
		Type: v.Type.ZFSVersionType(),
		Name: v.Name,
		Guid: v.Guid,
		CreateTXG: v.CreateTXG,
		Creation: ct,
	}
}