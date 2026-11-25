// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build windows

package toolrun

import (
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// group is a job object holding the child and everything it starts. The
// job is set to kill all of them when its handle closes, so even a crash
// of pentym stops the children.
type group struct{ job windows.Handle }

func newGroup() (*group, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil { // #nosec G103 -- the documented way to pass job limits
		_ = windows.CloseHandle(job)
		return nil, err
	}
	return &group{job: job}, nil
}

// prepare creates the child suspended, in its own process group so console
// signals aimed at pentym do not reach it directly. It runs only once it is
// in the job (see started), so nothing it starts can escape the job.
func (g *group) prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED}
}

// ntResumeProcess resumes every thread of a suspended process.
var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// started puts the suspended child in the job, then lets it run. Processes
// it starts join the job too.
func (g *group) started(cmd *exec.Cmd) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME, false, uint32(cmd.Process.Pid)) // #nosec G115 -- PIDs fit in 32 bits on Windows
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := windows.AssignProcessToJobObject(g.job, h); err != nil {
		return err
	}
	if status, _, _ := ntResumeProcess.Call(uintptr(h)); status != 0 {
		return fmt.Errorf("resume the process: NTSTATUS %#x", status)
	}
	return nil
}

// terminate stops every process in the job. Windows has no gentle stop for
// console tools that pentym could rely on.
func (g *group) terminate() error { return g.kill() }

func (g *group) kill() error { return windows.TerminateJobObject(g.job, 1) }

// close kills anything left in the job and releases it.
func (g *group) close() {
	_ = g.kill()
	_ = windows.CloseHandle(g.job)
}

// pidAlive reports whether a process exists.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid)) // #nosec G115 -- PIDs fit in 32 bits on Windows
	if err != nil {
		return err == windows.ERROR_ACCESS_DENIED
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == 259 // STILL_ACTIVE
}

// restrict replaces a file's or directory's DACL with one granting only the
// current user full control, without inherited entries.
func restrict(path string, _ bool) error {
	tok := windows.GetCurrentProcessToken()
	user, err := tok.GetTokenUser()
	if err != nil {
		return err
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}
