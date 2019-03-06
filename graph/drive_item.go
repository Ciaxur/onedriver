package graph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
)

// DriveItemParent describes a DriveItem's parent in the Graph API (just another
// DriveItem's ID and its path)
type DriveItemParent struct {
	ID   string `json:"id,omitempty"`
	Path string `json:"path,omitempty"`
	item *DriveItem
}

// DriveItem represents a file or folder fetched from the Graph API. All struct
// fields are pointers so as to avoid including them when marshaling to JSON.
type DriveItem struct {
	nodefs.File `json:"-"`
	auth        *Auth            // only populated for root item
	data        *[]byte          // empty by default
	hasChanges  bool             // used to trigger an upload on flush
	ID          string           `json:"id,omitempty"`
	Name        string           `json:"name,omitempty"`
	Size        uint64           `json:"size,omitempty"`
	ModifyTime  *time.Time       `json:"lastModifiedDatetime,omitempty"`
	mode        uint32           // do not set manually
	Parent      *DriveItemParent `json:"parentReference,omitempty"`
	children    map[string]*DriveItem
	Folder      *struct {
		ChildCount uint32 `json:"childCount,omitempty"`
	} `json:"folder,omitempty"`
	FileAPI *struct { // renamed to avoid conflict with nodefs.File interface
		MimeType string `json:"mimeType,omitempty"`
	} `json:"file,omitempty"`
}

// NewDriveItem initializes a new DriveItem
func NewDriveItem(name string, mode uint32, parent *DriveItem) *DriveItem {
	var empty []byte
	currentTime := time.Now()
	return &DriveItem{
		File: nodefs.NewDefaultFile(),
		Name: name,
		Parent: &DriveItemParent{
			ID:   parent.ID,
			Path: parent.Parent.Path + "/" + parent.Name,
			item: parent,
		},
		children:   make(map[string]*DriveItem),
		data:       &empty,
		ModifyTime: &currentTime,
		mode:       mode,
	}
}

func (d DriveItem) String() string {
	l := d.Size
	if l > 10 {
		l = 10
	}
	return fmt.Sprintf("DriveItem(%x)", (*d.data)[:l])
}

// Set an item's parent
func (d *DriveItem) setParent(newParent *DriveItem) {
	d.Parent = &DriveItemParent{
		ID:   newParent.ID,
		Path: newParent.Path(),
		item: newParent,
	}
}

// Path returns an item's full Path
func (d DriveItem) Path() string {
	// special case when it's the root item
	if d.Parent.Path == "" && d.Name == "root" {
		return "/"
	}
	// all paths come prefixed with "/drive/root:"
	return strings.TrimPrefix(d.Parent.Path+"/"+d.Name, "/drive/root:")
}

// only used for parsing
type driveChildren struct {
	Children []*DriveItem `json:"value"`
}

// GetChildren fetches all DriveItems that are children of resource at path.
// Also initializes the children field.
func (d *DriveItem) GetChildren(auth Auth) (map[string]*DriveItem, error) {
	//TODO will exit prematurely if *any* children are in the cache
	if !d.IsDir() || d.children != nil {
		return d.children, nil
	}

	body, err := Get(ChildrenPath(d.Path()), auth)
	var fetched driveChildren
	if err != nil {
		return nil, err
	}
	json.Unmarshal(body, &fetched)

	d.children = make(map[string]*DriveItem)
	for _, child := range fetched.Children {
		child.Parent.item = d
		d.children[child.Name] = child
	}

	return d.children, nil
}

// FetchContent fetches a DriveItem's content and initializes the .Data field.
func (d *DriveItem) FetchContent(auth Auth) error {
	body, err := Get("/me/drive/items/"+d.ID+"/content", auth)
	if err != nil {
		return err
	}
	d.data = &body
	d.File = nodefs.NewDefaultFile()
	return nil
}

// Read from a DriveItem like a file
func (d DriveItem) Read(buf []byte, off int64) (res fuse.ReadResult, code fuse.Status) {
	end := int(off) + int(len(buf))
	if end > len(*d.data) {
		end = len(*d.data)
	}
	log.Printf("Read(\"%s\"): %d bytes at offset %d\n", d.Name, int64(end)-off, off)
	return fuse.ReadResultData((*d.data)[off:end]), fuse.OK
}

// Write to a DriveItem like a file. Note that changes are 100% local until
// Flush() is called.
func (d *DriveItem) Write(data []byte, off int64) (uint32, fuse.Status) {
	nWrite := len(data)
	offset := int(off)
	log.Printf("Write(\"%s\"): %d bytes at offset %d\n", d.Name, nWrite, off)

	if offset+nWrite > int(d.Size)-1 {
		// we've exceeded the file size, overwrite via append
		*d.data = append((*d.data)[:offset], data...)
	} else {
		// writing inside the current file, overwrite in place
		copy((*d.data)[offset:], data)
	}
	// probably a better way to do this, but whatever
	d.Size = uint64(len(*d.data))
	d.hasChanges = true

	return uint32(nWrite), fuse.OK
}

func (d DriveItem) getRoot() *DriveItem {
	parent := d.Parent.item
	for parent.Parent.Path != "" {
		parent = parent.Parent.item
	}
	return parent
}

// Flush is called when a file descriptor is closed, and is responsible for upload
func (d DriveItem) Flush() fuse.Status {
	log.Printf("Flush(\"%s\")\n", d.Name)
	if d.hasChanges {
		log.Println("Triggering upload of:", d.Name)
		auth := *d.getRoot().auth
		go d.Upload(auth)
	}
	return fuse.OK
}

// Upload copies the file's contents to the server
func (d *DriveItem) Upload(auth Auth) error {
	// TODO implement upload sessions for files over 4MB
	var uploadPath string
	if d.ID == "" { // ID will be empty for a file that's local only
		uploadPath = fmt.Sprintf("/me/drive/items/%s:/%s:/content",
			d.Parent.ID, d.Name)
	} else {
		uploadPath = "/me/drive/items/" + d.ID + "/content"
	}
	resp, err := Put(uploadPath, auth, bytes.NewReader(*d.data))
	if err != nil {
		return err
	}
	// Unmarshal into existing item so we don't have to redownload file contents.
	return json.Unmarshal(resp, d)
}

// GetAttr returns a the DriveItem as a UNIX stat
func (d DriveItem) GetAttr(out *fuse.Attr) fuse.Status {
	out.Size = d.FakeSize()
	out.Nlink = d.NLink()
	out.Atime = d.MTime()
	out.Mtime = d.MTime()
	out.Ctime = d.MTime()
	out.Mode = d.Mode()
	out.Owner = fuse.Owner{
		Uid: uint32(os.Getuid()),
		Gid: uint32(os.Getgid()),
	}
	return fuse.OK
}

// Utimens sets the access/modify times of a file
func (d *DriveItem) Utimens(atime *time.Time, mtime *time.Time) fuse.Status {
	d.ModifyTime = mtime
	return fuse.OK
}

// Truncate cuts a file in place
func (d *DriveItem) Truncate(size uint64) fuse.Status {
	*d.data = (*d.data)[:size]
	d.Size = size
	d.hasChanges = true
	return fuse.OK
}

// IsDir returns if it is a directory (true) or file (false).
func (d DriveItem) IsDir() bool {
	// following statement returns 0 if the dir bit is not set
	return d.Mode()&fuse.S_IFDIR > 0
}

// Mode returns the permissions/mode of the file. Lazily initializes the
// underlying mode field.
func (d *DriveItem) Mode() uint32 {
	if d.mode == 0 { // only 0 if fetched from Graph API
		if d.FileAPI == nil { // nil if a folder
			d.mode = fuse.S_IFDIR | 0755
		} else {
			d.mode = fuse.S_IFREG | 0644
		}
	}
	return d.mode
}

// MTime returns the Unix timestamp of last modification
func (d DriveItem) MTime() uint64 {
	return uint64(d.ModifyTime.Unix())
}

// NLink gives the number of hard links to an inode (or child count if a
// directory)
func (d DriveItem) NLink() uint32 {
	if d.IsDir() {
		// technically 2 + number of subdirectories
		var nSubdir uint32
		for _, v := range d.children {
			if v.IsDir() {
				nSubdir++
			}
		}
		return 2 + nSubdir
	}
	return 1
}

// FakeSize pretends that folders are 4096 bytes, even though they're 0 (since
// they actually don't exist).
func (d DriveItem) FakeSize() uint64 {
	if d.IsDir() {
		return 4096
	}
	return d.Size
}
