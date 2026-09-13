//! A suspended child joins a kill-on-close job before any of its code runs.
use std::{io, mem::size_of, os::windows::io::AsRawHandle, process::Child, ptr};
use windows_sys::Win32::{
    Foundation::{CloseHandle, HANDLE, INVALID_HANDLE_VALUE},
    System::{
        Diagnostics::ToolHelp::{
            CreateToolhelp32Snapshot, Thread32First, Thread32Next, TH32CS_SNAPTHREAD, THREADENTRY32,
        },
        JobObjects::{
            AssignProcessToJobObject, CreateJobObjectW, JobObjectExtendedLimitInformation,
            SetInformationJobObject, JOBOBJECT_EXTENDED_LIMIT_INFORMATION,
            JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
        },
        Threading::{OpenThread, ResumeThread, THREAD_SUSPEND_RESUME},
    },
};

struct Handle(HANDLE);
impl Drop for Handle {
    fn drop(&mut self) {
        // SAFETY: each Handle owns one successful native allocation.
        unsafe {
            CloseHandle(self.0);
        }
    }
}

pub(super) struct Job {
    _handle: Handle,
}
impl Job {
    pub(super) fn attach_and_resume(child: &Child) -> io::Result<Self> {
        // SAFETY: pointers reference initialized structures of the declared size;
        // owned handles are closed on every return path. The process was spawned
        // suspended, so there is exactly one primary thread to resume.
        unsafe {
            let raw = CreateJobObjectW(ptr::null(), ptr::null());
            if raw.is_null() {
                return Err(io::Error::last_os_error());
            }
            let job = Self {
                _handle: Handle(raw),
            };
            let mut limits = JOBOBJECT_EXTENDED_LIMIT_INFORMATION::default();
            limits.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
            if SetInformationJobObject(
                raw,
                JobObjectExtendedLimitInformation,
                &limits as *const _ as *const _,
                size_of::<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>() as u32,
            ) == 0
            {
                return Err(io::Error::last_os_error());
            }
            if AssignProcessToJobObject(raw, child.as_raw_handle()) == 0 {
                return Err(io::Error::last_os_error());
            }
            let raw_snapshot = CreateToolhelp32Snapshot(TH32CS_SNAPTHREAD, 0);
            if raw_snapshot == INVALID_HANDLE_VALUE {
                return Err(io::Error::last_os_error());
            }
            let snapshot = Handle(raw_snapshot);
            let mut entry = THREADENTRY32 {
                dwSize: size_of::<THREADENTRY32>() as u32,
                ..Default::default()
            };
            let mut next = Thread32First(snapshot.0, &mut entry);
            while next != 0 {
                if entry.th32OwnerProcessID == child.id() {
                    let raw_thread = OpenThread(THREAD_SUSPEND_RESUME, 0, entry.th32ThreadID);
                    if raw_thread.is_null() {
                        return Err(io::Error::last_os_error());
                    }
                    let thread = Handle(raw_thread);
                    if ResumeThread(thread.0) == u32::MAX {
                        return Err(io::Error::last_os_error());
                    }
                    return Ok(job);
                }
                entry.dwSize = size_of::<THREADENTRY32>() as u32;
                next = Thread32Next(snapshot.0, &mut entry);
            }
            Err(io::Error::other("suspended command has no primary thread"))
        }
    }
}
