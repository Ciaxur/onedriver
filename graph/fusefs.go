package graph

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"regexp"

	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"github.com/hanwen/go-fuse/fuse/pathfs"
)

// these files will never exist, and we should ignore them
func ignore(path string) bool {
	ignoredFiles := []string{
		"/BDMV",
		"/.Trash",
		"/.Trash-1000",
		"/.xdg-volume-info",
		"/autorun.inf",
		"/.localized",
		"/.DS_Store",
		"/._.",
		"/.hidden",
	}
	for _, ignore := range ignoredFiles {
		if path == ignore {
			return true
		}
	}
	return false
}

// FuseFs is a memory-backed filesystem for Microsoft Graph
type FuseFs struct {
	pathfs.FileSystem
	Auth Auth
}

// GetAttr returns a stat structure for the specified file
func (fs *FuseFs) GetAttr(name string, context *fuse.Context) (*fuse.Attr, fuse.Status) {
	name = "/" + name
	if ignore(name) {
		return nil, fuse.ENOENT
	}
	log.Printf("GetAttr(\"%s\")\n", name)
	item, err := GetItem(name, fs.Auth)
	if err != nil {
		return nil, fuse.ENOENT
	}

	// convert to UNIX struct stat
	attr := fuse.Attr{
		Size:  item.FakeSize(),
		Atime: item.MTime(),
		Mtime: item.MTime(),
		Ctime: item.MTime(),
		Mode:  item.Mode(),
		Owner: fuse.Owner{
			Uid: uint32(os.Getuid()),
			Gid: uint32(os.Getgid()),
		},
	}
	return &attr, fuse.OK
}

// Chown currently does nothing - it is not a valid option, since fuse is single-user anyways
func (fs *FuseFs) Chown(name string, uid uint32, gid uint32, context *fuse.Context) (code fuse.Status) {
	return fuse.EPERM
}

// Chmod currently does nothing - no way to change mode yet.
func (fs *FuseFs) Chmod(name string, mode uint32, context *fuse.Context) (code fuse.Status) {
	return fuse.EPERM
}

// OpenDir returns a list of directory entries
func (fs *FuseFs) OpenDir(name string, context *fuse.Context) (c []fuse.DirEntry, code fuse.Status) {
	name = "/" + name
	log.Printf("OpenDir(\"%s\")\n", name)
	children, err := GetChildren(name, fs.Auth)
	if err != nil {
		// that directory probably doesn't exist. silly human.
		return nil, fuse.ENOENT
	}
	for _, child := range children {
		entry := fuse.DirEntry{
			Name: child.Name,
			Mode: child.Mode(),
		}
		c = append(c, entry)
	}
	return c, fuse.OK
}

type newFolderPost struct {
	Name   string   `json:"name"`
	Folder struct{} `json:"folder"`
}

// Mkdir creates a directory, mode is ignored
func (fs *FuseFs) Mkdir(name string, mode uint32, context *fuse.Context) fuse.Status {
	name = "/" + name
	log.Printf("Mkdir(\"%s\")\n", name)
	if name[len(name)-1] == '/' {
		// remove trailing slash (if) exists for easier parsing later
		name = name[:len(name)-1]
	}
	re := regexp.MustCompile(`\w+$`)
	split := re.FindStringIndex(name)[0]
	parent, child := name[:split], name[split:]

	bytePayload, _ := json.Marshal(newFolderPost{Name: child})
	resp, err := Post(ChildrenPath(parent), fs.Auth, bytes.NewReader(bytePayload))
	if err != nil {
		log.Println(string(resp))
		log.Println(err)
		return fuse.EREMOTEIO
	}
	return fuse.OK
}

// Rmdir removes a directory
func (fs *FuseFs) Rmdir(name string, context *fuse.Context) fuse.Status {
	//TODO use as a general delete item method?
	name = "/" + name
	log.Printf("Rmdir(\"%s\")\n", name)
	item, err := GetItem(name, fs.Auth)
	if err != nil {
		log.Println(err)
		return fuse.EREMOTEIO
	}
	err = Delete("/me/drive/items/"+item.ID, fs.Auth)
	if err != nil {
		log.Println(err)
		return fuse.EREMOTEIO
	}
	return fuse.OK
}

// Open returns a file that can be read and written to
func (fs *FuseFs) Open(name string, flags uint32, context *fuse.Context) (file nodefs.File, code fuse.Status) {
	name = "/" + name
	log.Printf("Open(\"%s\")\n", name)
	item, err := GetItem(name, fs.Auth)
	if err != nil {
		// doesn't exist or internet is out - either way, no files for you!
		return nil, fuse.ENOENT
	}

	//TODO deny write permissions until uploads/writes are implemented
	if flags&fuse.O_ANYWRITE != 0 {
		return nil, fuse.EPERM
	}

	body, err := Get("/me/drive/items/"+item.ID+"/content", fs.Auth)
	if err != nil {
		log.Printf("Failed to fetch content for '%s': %s\n", item.ID, err)
		return nil, fuse.ENOENT
	}
	//TODO this is a read-only file - will need to implement our own version of
	// the File interface for write functionality
	return nodefs.NewDataFile(body), fuse.OK
}