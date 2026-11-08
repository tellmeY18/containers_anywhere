package backuprepo

import (
	"io"
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Inspect security at the beginning of the copy, while the named destination
// is still empty: no post-creation ACL narrowing can satisfy this assertion.
func TestPrivateCopyHasOwnerOnlyACLBeforeFirstByte(t *testing.T) {
	root, err := openRoot(realTemp(t))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	source, name, err := temporary(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.WriteString("secret key bytes"); err != nil {
		t.Fatal(err)
	}
	source.Close()
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	err = publishWithCreate(root, name, "key", noHardlinks, func(dst io.Writer, src io.Reader) (int64, error) {
		f := dst.(*os.File)
		info, err := f.Stat()
		if err != nil || info.Size() != 0 {
			t.Fatalf("private file not inspected before copy: %v", err)
		}
		sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		control, _, err := sd.Control()
		if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
			t.Fatalf("new key inherited its DACL: %v", err)
		}
		acl, _, err := sd.DACL()
		if err != nil || acl == nil || acl.AceCount != 1 {
			t.Fatalf("new key has a permissive DACL: %v", err)
		}
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, 0, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || sid.String() != user.User.Sid.String() {
			t.Fatal("new key permits someone other than its owner")
		}
		return io.Copy(dst, src)
	}, createPrivateFile)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := createPrivateFile(root, "key"); err == nil {
		f.Close()
		t.Fatal("private key was overwritten")
	}
}
