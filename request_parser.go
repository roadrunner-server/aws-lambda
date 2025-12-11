package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
)

const (
	defaultMaxMemory = 32 << 20 // 32 MB
	contentNone      = iota + 900
	contentStream
	contentMultipart
	contentURLEncoded

	uploadErrorOK        = 0
	uploadErrorNoFile    = 4
	uploadErrorNoTmpDir  = 6
	uploadErrorCantWrite = 7
	uploadErrorExtension = 8
	tempFilePattern      = "upload"
)

type (
	dataTree map[string]any
	fileTree map[string]any
)

const maxLevel = 127

type Uploads struct {
	tree fileTree
	list []*FileUpload
}

func (u *Uploads) MarshalJSON() ([]byte, error) {
	return json.Marshal(u.tree)
}

// Clear deletes all temporary files created while handling multipart uploads.
func (u *Uploads) Clear() {
	for _, f := range u.list {
		if f.TempFilename != "" && exists(f.TempFilename) {
			_ = os.Remove(f.TempFilename)
		}
	}
}

type FileUpload struct {
	Name         string `json:"name"`
	Mime         string `json:"mime"`
	Size         int64  `json:"size"`
	Error        int    `json:"error"`
	TempFilename string `json:"tmpName"`

	header *multipart.FileHeader
}

func NewUpload(f *multipart.FileHeader) *FileUpload {
	return &FileUpload{
		Name:   f.Filename,
		Mime:   f.Header.Get("Content-Type"),
		Error:  uploadErrorOK,
		header: f,
	}
}

func (f *FileUpload) Open() error {
	file, err := f.header.Open()
	if err != nil {
		f.Error = uploadErrorNoFile
		return err
	}
	defer file.Close()

	tmp, err := os.CreateTemp("", tempFilePattern)
	if err != nil {
		f.Error = uploadErrorNoTmpDir
		return err
	}
	defer tmp.Close()

	f.TempFilename = tmp.Name()
	f.Size, err = io.Copy(tmp, file)
	if err != nil {
		f.Error = uploadErrorCantWrite
		return err
	}

	return nil
}

func exists(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

func contentType(method, ct string) int {
	if method == http.MethodHead || method == http.MethodOptions {
		return contentNone
	}

	switch {
	case strings.Contains(ct, "application/x-www-form-urlencoded"):
		return contentURLEncoded
	case strings.Contains(ct, "multipart/form-data"):
		return contentMultipart
	default:
		return contentStream
	}
}

func parseURLEncoded(body []byte, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	for k, v := range headers {
		req.Header.Add(k, v)
	}

	if err := req.ParseForm(); err != nil {
		return nil, err
	}

	data, err := parsePostForm(req)
	if err != nil {
		return nil, err
	}

	return packDataTree(data)
}

func parseMultipart(body []byte, headers map[string]string) ([]byte, *Uploads, error) {
	req, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	for k, v := range headers {
		req.Header.Add(k, v)
	}
	req.ContentLength = int64(len(body))

	if err := req.ParseMultipartForm(defaultMaxMemory); err != nil {
		return nil, nil, err
	}

	uploads, err := parseUploads(req)
	if err != nil {
		return nil, nil, err
	}
	for _, f := range uploads.list {
		_ = f.Open()
	}

	data, err := parseMultipartData(req)
	if err != nil {
		return nil, nil, err
	}

	encoded, err := packDataTree(data)
	if err != nil {
		return nil, nil, err
	}

	return encoded, uploads, nil
}

func parsePostForm(r *http.Request) (dataTree, error) {
	data := make(dataTree, 2)

	if r.PostForm != nil {
		for k, v := range r.PostForm {
			if err := data.push(k, v); err != nil {
				return nil, err
			}
		}
	}

	return data, nil
}

func parseMultipartData(r *http.Request) (dataTree, error) {
	data := make(dataTree, 2)

	if r.MultipartForm != nil {
		for k, v := range r.MultipartForm.Value {
			if err := data.push(k, v); err != nil {
				return nil, err
			}
		}
	}

	return data, nil
}

func parseUploads(r *http.Request) (*Uploads, error) {
	u := &Uploads{
		tree: make(fileTree),
		list: make([]*FileUpload, 0),
	}

	if r.MultipartForm == nil {
		return u, nil
	}

	for k, v := range r.MultipartForm.File {
		files := make([]*FileUpload, 0, len(v))
		for _, f := range v {
			files = append(files, NewUpload(f))
		}

		u.list = append(u.list, files...)
		if err := u.tree.push(k, files); err != nil {
			return nil, err
		}
	}

	return u, nil
}

func packDataTree(t dataTree) ([]byte, error) {
	if len(t) == 0 {
		return []byte("{}"), nil
	}

	return json.Marshal(t)
}

// Below functions mirror the tree building logic from roadrunner-server-http.

func (dt dataTree) push(k string, v []string) error {
	keys := make([]string, 1)
	fetchIndexes(k, &keys)
	if len(keys) <= maxLevel {
		return dt.mount(keys, v)
	}

	return nil
}

func (ft fileTree) push(k string, v []*FileUpload) error {
	keys := make([]string, 1)
	fetchIndexes(k, &keys)
	if len(keys) <= maxLevel {
		return ft.mount(keys, v)
	}
	return nil
}

func (dt dataTree) mount(keys, v []string) error {
	if len(keys) == 0 {
		return nil
	}

	done, err := prepareTreeNode(dt, keys, v)
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	if len(keys) == 2 && keys[1] == "" {
		dt[keys[0]] = v
		return nil
	}
	if len(keys) == 1 && len(v) > 0 {
		dt[keys[0]] = v[len(v)-1]
		return nil
	}
	if len(keys) == 1 {
		dt[keys[0]] = v
		return nil
	}

	res, ok := dt[keys[0]].(dataTree)
	if !ok {
		dt[keys[0]] = make(dataTree, 1)
		res, ok = dt[keys[0]].(dataTree)
		if !ok {
			return nil
		}
	}

	return res.mount(keys[1:], v)
}

func (ft fileTree) mount(i []string, v []*FileUpload) error {
	if len(i) == 0 {
		return nil
	}

	done, err := prepareTreeNode(ft, i, v)
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	switch {
	case len(i) == 2 && i[1] == "":
		ft[i[0]] = v
		return nil
	case len(i) == 1 && len(v) > 0:
		ft[i[0]] = v[0]
		return nil
	case len(i) == 1:
		ft[i[0]] = v
		return nil
	}

	sub, ok := ft[i[0]].(fileTree)
	if !ok {
		return errors.New("invalid tree structure: expected fileTree")
	}
	return sub.mount(i[1:], v)
}

func fetchIndexes(s string, keys *[]string) {
	const empty = ""
	var (
		pos int
		ch  string
	)

	for _, c := range s {
		ch = string(c)
		switch ch {
		case " ":
			continue
		case "[":
			pos = 1
			continue
		case "]":
			if pos == 1 {
				*keys = append(*keys, empty)
			}
			pos = 2
		default:
			if pos == 1 || pos == 2 {
				*keys = append(*keys, empty)
			}

			(*keys)[len(*keys)-1] += ch
			pos = 0
		}
	}
}

func prepareTreeNode[T dataTree | fileTree, V []string | []*FileUpload](tree T, i []string, v V) (bool, error) {
	if _, ok := tree[i[0]]; !ok {
		tree[i[0]] = make(T)
		return false, nil
	}

	_, isBranch := tree[i[0]].(T)
	isDataInTreeEmpty := isDataEmpty(tree[i[0]])
	isIncomingValueEmpty := isDataEmpty(v)
	isLeafNodeIncoming := len(i) == 1 || (len(i) == 2 && len(i[1]) == 0)

	if !isBranch {
		if !isDataInTreeEmpty {
			if len(i) > 1 && len(i[1]) > 0 {
				return true, errors.New("invalid multiple values in tree")
			}

			if isIncomingValueEmpty {
				return true, nil
			}
		}

		if isDataInTreeEmpty && !isIncomingValueEmpty {
			tree[i[0]] = make(T)
			return false, nil
		}
	}

	if isBranch && isLeafNodeIncoming {
		if !isIncomingValueEmpty {
			return true, errors.New("invalid multiple values in tree")
		}

		if isIncomingValueEmpty {
			return true, nil
		}
	}

	return false, nil
}

func isDataEmpty(v any) bool {
	switch actualV := v.(type) {
	case string:
		return len(actualV) == 0
	case []string:
		return len(actualV) == 0 || (len(actualV) == 1 && len(actualV[0]) == 0)
	case []*FileUpload:
		return len(actualV) == 0 || (len(actualV) == 1 && actualV[0] == nil)
	default:
		return v == nil
	}
}
