// Copyright 2014 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package models

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Unknwon/com"

	"github.com/gogits/gogs/modules/log"
	"github.com/gogits/gogs/modules/process"
	"github.com/gogits/gogs/modules/setting"
)

const (
	// "### autogenerated by gitgos, DO NOT EDIT\n"
	_TPL_PUBLICK_KEY = `command="%s serv key-%d --config='%s'",no-port-forwarding,no-X11-forwarding,no-agent-forwarding,no-pty %s` + "\n"
)

var (
	ErrKeyAlreadyExist = errors.New("Public key already exists")
	ErrKeyNotExist     = errors.New("Public key does not exist")
	ErrKeyUnableVerify = errors.New("Unable to verify public key")
)

var sshOpLocker = sync.Mutex{}

var (
	SSHPath string // SSH directory.
	appPath string // Execution(binary) path.
)

// exePath returns the executable path.
func exePath() (string, error) {
	file, err := exec.LookPath(os.Args[0])
	if err != nil {
		return "", err
	}
	return filepath.Abs(file)
}

// homeDir returns the home directory of current user.
func homeDir() string {
	home, err := com.HomeDir()
	if err != nil {
		log.Fatal(4, "Fail to get home directory: %v", err)
	}
	return home
}

func init() {
	var err error

	if appPath, err = exePath(); err != nil {
		log.Fatal(4, "fail to get app path: %v\n", err)
	}
	appPath = strings.Replace(appPath, "\\", "/", -1)

	// Determine and create .ssh path.
	SSHPath = filepath.Join(homeDir(), ".ssh")
	if err = os.MkdirAll(SSHPath, 0700); err != nil {
		log.Fatal(4, "fail to create '%s': %v", SSHPath, err)
	}
}

// PublicKey represents a SSH key.
type PublicKey struct {
	Id                int64
	OwnerId           int64     `xorm:"UNIQUE(s) INDEX NOT NULL"`
	Name              string    `xorm:"UNIQUE(s) NOT NULL"`
	Fingerprint       string    `xorm:"INDEX NOT NULL"`
	Content           string    `xorm:"TEXT NOT NULL"`
	Created           time.Time `xorm:"CREATED"`
	Updated           time.Time
	HasRecentActivity bool `xorm:"-"`
	HasUsed           bool `xorm:"-"`
}

// OmitEmail returns content of public key but without e-mail address.
func (k *PublicKey) OmitEmail() string {
	return strings.Join(strings.Split(k.Content, " ")[:2], " ")
}

// GetAuthorizedString generates and returns formatted public key string for authorized_keys file.
func (key *PublicKey) GetAuthorizedString() string {
	return fmt.Sprintf(_TPL_PUBLICK_KEY, appPath, key.Id, setting.CustomConf, key.Content)
}

var (
	MinimumKeySize = map[string]int{
		"(ED25519)": 256,
		"(ECDSA)":   256,
		"(NTRU)":    1087,
		"(MCE)":     1702,
		"(McE)":     1702,
		"(RSA)":     2048,
		"(DSA)":     1024,
	}
)

func extractTypeFromBase64Key(key string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(b) < 4 {
		return "", errors.New("Invalid key format")
	}

	keyLength := int(binary.BigEndian.Uint32(b))

	if len(b) < 4+keyLength {
		return "", errors.New("Invalid key format")
	}

	return string(b[4 : 4+keyLength]), nil
}

// Parse any key string in openssh or ssh2 format to clean openssh string (rfc4253)
func ParseKeyString(content string) (string, error) {
	// Transform all legal line endings to a single "\n"
	s := strings.Replace(strings.Replace(strings.TrimSpace(content), "\r\n", "\n", -1), "\r", "\n", -1)

	lines := strings.Split(s, "\n")

	var keyType, keyContent, keyComment string

	if len(lines) == 1 {
		// Parse openssh format
		parts := strings.Fields(lines[0])
		switch len(parts) {
		case 0:
			return "", errors.New("Empty key")
		case 1:
			keyContent = parts[0]
		case 2:
			keyType = parts[0]
			keyContent = parts[1]
		default:
			keyType = parts[0]
			keyContent = parts[1]
			keyComment = parts[2]
		}

		// If keyType is not given, extract it from content. If given, validate it
		if len(keyType) == 0 {
			if t, err := extractTypeFromBase64Key(keyContent); err == nil {
				keyType = t
			} else {
				return "", err
			}
		} else {
			if t, err := extractTypeFromBase64Key(keyContent); err != nil || keyType != t {
				return "", err
			}
		}
	} else {
		// Parse SSH2 file format.
		continuationLine := false

		for _, line := range lines {
			// Skip lines that:
			// 1) are a continuation of the previous line,
			// 2) contain ":" as that are comment lines
			// 3) contain "-" as that are begin and end tags
			if continuationLine || strings.ContainsAny(line, ":-") {
				continuationLine = strings.HasSuffix(line, "\\")
			} else {
				keyContent = keyContent + line
			}
		}

		if t, err := extractTypeFromBase64Key(keyContent); err == nil {
			keyType = t
		} else {
			return "", err
		}
	}
	return keyType + " " + keyContent + " " + keyComment, nil
}

// CheckPublicKeyString checks if the given public key string is recognized by SSH.
func CheckPublicKeyString(content string) (bool, error) {
	content = strings.TrimRight(content, "\n\r")
	if strings.ContainsAny(content, "\n\r") {
		return false, errors.New("only a single line with a single key please")
	}

	// write the key to a file…
	tmpFile, err := ioutil.TempFile(os.TempDir(), "keytest")
	if err != nil {
		return false, err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	tmpFile.WriteString(content)
	tmpFile.Close()

	// Check if ssh-keygen recognizes its contents.
	stdout, stderr, err := process.Exec("CheckPublicKeyString", "ssh-keygen", "-l", "-f", tmpPath)
	if err != nil {
		return false, errors.New("ssh-keygen -l -f: " + stderr)
	} else if len(stdout) < 2 {
		return false, errors.New("ssh-keygen returned not enough output to evaluate the key: " + stdout)
	}

	// The ssh-keygen in Windows does not print key type, so no need go further.
	if setting.IsWindows {
		return true, nil
	}

	fmt.Println(stdout)
	sshKeygenOutput := strings.Split(stdout, " ")
	if len(sshKeygenOutput) < 4 {
		return false, ErrKeyUnableVerify
	}

	// Check if key type and key size match.
	keySize := com.StrTo(sshKeygenOutput[0]).MustInt()
	if keySize == 0 {
		return false, errors.New("cannot get key size of the given key")
	}
	keyType := strings.TrimSpace(sshKeygenOutput[len(sshKeygenOutput)-1])
	if minimumKeySize := MinimumKeySize[keyType]; minimumKeySize == 0 {
		return false, errors.New("sorry, unrecognized public key type")
	} else if keySize < minimumKeySize {
		return false, fmt.Errorf("the minimum accepted size of a public key %s is %d", keyType, minimumKeySize)
	}

	return true, nil
}

// saveAuthorizedKeyFile writes SSH key content to authorized_keys file.
func saveAuthorizedKeyFile(keys ...*PublicKey) error {
	sshOpLocker.Lock()
	defer sshOpLocker.Unlock()

	fpath := filepath.Join(SSHPath, "authorized_keys")
	f, err := os.OpenFile(fpath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	finfo, err := f.Stat()
	if err != nil {
		return err
	}

	// FIXME: following command does not support in Windows.
	if !setting.IsWindows {
		if finfo.Mode().Perm() > 0600 {
			log.Error(4, "authorized_keys file has unusual permission flags: %s - setting to -rw-------", finfo.Mode().Perm().String())
			if err = f.Chmod(0600); err != nil {
				return err
			}
		}
	}

	for _, key := range keys {
		if _, err = f.WriteString(key.GetAuthorizedString()); err != nil {
			return err
		}
	}
	return nil
}

// AddPublicKey adds new public key to database and authorized_keys file.
func AddPublicKey(key *PublicKey) (err error) {
	has, err := x.Get(key)
	if err != nil {
		return err
	} else if has {
		return ErrKeyAlreadyExist
	}

	// Calculate fingerprint.
	tmpPath := strings.Replace(path.Join(os.TempDir(), fmt.Sprintf("%d", time.Now().Nanosecond()),
		"id_rsa.pub"), "\\", "/", -1)
	os.MkdirAll(path.Dir(tmpPath), os.ModePerm)
	if err = ioutil.WriteFile(tmpPath, []byte(key.Content), os.ModePerm); err != nil {
		return err
	}
	stdout, stderr, err := process.Exec("AddPublicKey", "ssh-keygen", "-l", "-f", tmpPath)
	if err != nil {
		return errors.New("ssh-keygen -l -f: " + stderr)
	} else if len(stdout) < 2 {
		return errors.New("not enough output for calculating fingerprint: " + stdout)
	}
	key.Fingerprint = strings.Split(stdout, " ")[1]
	if has, err := x.Get(&PublicKey{Fingerprint: key.Fingerprint}); err == nil && has {
		return ErrKeyAlreadyExist
	}

	// Save SSH key.
	if _, err = x.Insert(key); err != nil {
		return err
	} else if err = saveAuthorizedKeyFile(key); err != nil {
		// Roll back.
		if _, err2 := x.Delete(key); err2 != nil {
			return err2
		}
		return err
	}

	return nil
}

// GetPublicKeyById returns public key by given ID.
func GetPublicKeyById(keyId int64) (*PublicKey, error) {
	key := new(PublicKey)
	has, err := x.Id(keyId).Get(key)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrKeyNotExist
	}
	return key, nil
}

// ListPublicKeys returns a list of public keys belongs to given user.
func ListPublicKeys(uid int64) ([]*PublicKey, error) {
	keys := make([]*PublicKey, 0, 5)
	err := x.Where("owner_id=?", uid).Find(&keys)
	if err != nil {
		return nil, err
	}

	for _, key := range keys {
		key.HasUsed = key.Updated.After(key.Created)
		key.HasRecentActivity = key.Updated.Add(7 * 24 * time.Hour).After(time.Now())
	}
	return keys, nil
}

// rewriteAuthorizedKeys finds and deletes corresponding line in authorized_keys file.
func rewriteAuthorizedKeys(key *PublicKey, p, tmpP string) error {
	sshOpLocker.Lock()
	defer sshOpLocker.Unlock()

	fr, err := os.Open(p)
	if err != nil {
		return err
	}
	defer fr.Close()

	fw, err := os.OpenFile(tmpP, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer fw.Close()

	isFound := false
	keyword := fmt.Sprintf("key-%d", key.Id)
	buf := bufio.NewReader(fr)
	for {
		line, errRead := buf.ReadString('\n')
		line = strings.TrimSpace(line)

		if errRead != nil {
			if errRead != io.EOF {
				return errRead
			}

			// Reached end of file, if nothing to read then break,
			// otherwise handle the last line.
			if len(line) == 0 {
				break
			}
		}

		// Found the line and copy rest of file.
		if !isFound && strings.Contains(line, keyword) && strings.Contains(line, key.Content) {
			isFound = true
			continue
		}
		// Still finding the line, copy the line that currently read.
		if _, err = fw.WriteString(line + "\n"); err != nil {
			return err
		}

		if errRead == io.EOF {
			break
		}
	}
	return nil
}

// UpdatePublicKey updates given public key.
func UpdatePublicKey(key *PublicKey) error {
	_, err := x.Id(key.Id).AllCols().Update(key)
	return err
}

// DeletePublicKey deletes SSH key information both in database and authorized_keys file.
func DeletePublicKey(key *PublicKey) error {
	has, err := x.Get(key)
	if err != nil {
		return err
	} else if !has {
		return ErrKeyNotExist
	}

	if _, err = x.Delete(key); err != nil {
		return err
	}

	fpath := filepath.Join(SSHPath, "authorized_keys")
	tmpPath := filepath.Join(SSHPath, "authorized_keys.tmp")
	if err = rewriteAuthorizedKeys(key, fpath, tmpPath); err != nil {
		return err
	} else if err = os.Remove(fpath); err != nil {
		return err
	}
	return os.Rename(tmpPath, fpath)
}

// RewriteAllPublicKeys removes any authorized key and rewrite all keys from database again.
func RewriteAllPublicKeys() error {
	sshOpLocker.Lock()
	defer sshOpLocker.Unlock()

	tmpPath := filepath.Join(SSHPath, "authorized_keys.tmp")
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)

	err = x.Iterate(new(PublicKey), func(idx int, bean interface{}) (err error) {
		_, err = f.WriteString((bean.(*PublicKey)).GetAuthorizedString())
		return err
	})
	f.Close()
	if err != nil {
		return err
	}

	fpath := filepath.Join(SSHPath, "authorized_keys")
	if com.IsExist(fpath) {
		if err = os.Remove(fpath); err != nil {
			return err
		}
	}
	if err = os.Rename(tmpPath, fpath); err != nil {
		return err
	}

	return nil
}
