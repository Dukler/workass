package acp

import (
	"encoding/binary"
	"sort"
	"syscall"
	"unsafe"
)

// The kernel call behind libproc. It answers "who holds this file" in about 20 ms
// where lsof takes about a second of the 1500 ms it is given, so a Mac with a few
// busy processes ran lsof out of time on every look: a Stop refused and a lane's
// process stayed unknown. Only what the daemon's user may see, the same as lsof.
const (
	sysProcInfo            = 336 // SYS_proc_info
	procInfoCallListPIDs   = 1   // PROC_INFO_CALL_LISTPIDS
	procInfoCallPIDInfo    = 2   // PROC_INFO_CALL_PIDINFO
	procInfoCallPIDFDInfo  = 3   // PROC_INFO_CALL_PIDFDINFO
	procAllPIDs            = 1   // PROC_ALL_PIDS
	procPIDListFDs         = 1   // PROC_PIDLISTFDS
	procPIDFDVnodeInfo     = 1   // PROC_PIDFDVNODEINFO
	proxFDTypeVnode        = 1   // PROX_FDTYPE_VNODE
	procFDInfoSize         = 8   // sizeof(struct proc_fdinfo)
	vnodeFDInfoSize        = 176 // sizeof(struct vnode_fdinfo)
	vnodeFDInfoDevOffset   = 24  // offsetof pvi.vi_stat.vst_dev
	vnodeFDInfoInodeOffset = 32  // offsetof pvi.vi_stat.vst_ino
)

func spawnedWorkProbeAvailable() bool { return true }

func spawnedWorkPIDsForOutputs(paths []string) (map[string][]int, bool) {
	if result, ok := darwinPIDsHoldingFiles(paths); ok {
		return result, true
	}
	return spawnedWorkPIDsForOutputsLsof(paths)
}

func procInfoCall(call, pid, flavor uintptr, arg int32, buffer []byte) (int, syscall.Errno) {
	var pointer unsafe.Pointer
	if len(buffer) > 0 {
		pointer = unsafe.Pointer(&buffer[0])
	}
	n, _, errno := syscall.Syscall6(sysProcInfo, call, pid, flavor, uintptr(arg), uintptr(pointer), uintptr(len(buffer)))
	return int(n), errno
}

// darwinAllPIDs lists every process, growing past any that started since the size
// was asked for. false means the call itself failed, not that nothing is running.
func darwinAllPIDs() ([]int32, bool) {
	size, errno := procInfoCall(procInfoCallListPIDs, procAllPIDs, 0, 0, nil)
	if errno != 0 || size <= 0 {
		return nil, false
	}
	for {
		buffer := make([]byte, size+64*4)
		used, errno := procInfoCall(procInfoCallListPIDs, procAllPIDs, 0, 0, buffer)
		if errno != 0 || used <= 0 {
			return nil, false
		}
		if used == len(buffer) {
			size = 2 * len(buffer)
			continue
		}
		pids := make([]int32, 0, used/4)
		for offset := 0; offset+4 <= used; offset += 4 {
			if pid := int32(binary.LittleEndian.Uint32(buffer[offset:])); pid > 0 {
				pids = append(pids, pid)
			}
		}
		return pids, true
	}
}

// darwinListFDs fills buffer with one process's open files. A process that exited or
// isn't this user's answers with an error, and holds nothing as far as anyone can tell.
func darwinListFDs(pid int32, buffer []byte) (int, []byte) {
	for {
		used, errno := procInfoCall(procInfoCallPIDInfo, uintptr(pid), procPIDListFDs, 0, buffer)
		if errno != 0 || used <= 0 {
			return 0, buffer
		}
		if used < len(buffer) {
			return used, buffer
		}
		buffer = make([]byte, 2*len(buffer))
	}
}

// darwinPIDsHoldingFiles matches on device and inode, so a symlinked path or a file
// renamed under its holder is still the same file. A path that doesn't exist has no
// holder, as with lsof.
func darwinPIDsHoldingFiles(paths []string) (map[string][]int, bool) {
	type fileID struct {
		device uint32
		inode  uint64
	}
	result := make(map[string][]int, len(paths))
	wanted := make(map[fileID][]string, len(paths))
	for _, path := range paths {
		result[path] = []int{}
		var stat syscall.Stat_t
		if syscall.Stat(path, &stat) != nil {
			continue
		}
		id := fileID{device: uint32(stat.Dev), inode: stat.Ino}
		wanted[id] = append(wanted[id], path)
	}
	if len(wanted) == 0 {
		return result, true
	}
	pids, ok := darwinAllPIDs()
	if !ok {
		return nil, false
	}
	seen := make(map[string]map[int]struct{}, len(paths))
	buffer := make([]byte, 64*1024)
	var vnode [vnodeFDInfoSize]byte
	for _, pid := range pids {
		var used int
		used, buffer = darwinListFDs(pid, buffer)
		for offset := 0; offset+procFDInfoSize <= used; offset += procFDInfoSize {
			if binary.LittleEndian.Uint32(buffer[offset+4:]) != proxFDTypeVnode {
				continue
			}
			fd := int32(binary.LittleEndian.Uint32(buffer[offset:]))
			n, errno := procInfoCall(procInfoCallPIDFDInfo, uintptr(pid), procPIDFDVnodeInfo, fd, vnode[:])
			if errno != 0 || n < vnodeFDInfoSize {
				continue // closed since it was listed
			}
			id := fileID{
				device: binary.LittleEndian.Uint32(vnode[vnodeFDInfoDevOffset:]),
				inode:  binary.LittleEndian.Uint64(vnode[vnodeFDInfoInodeOffset:]),
			}
			for _, path := range wanted[id] {
				if seen[path] == nil {
					seen[path] = map[int]struct{}{}
				}
				seen[path][int(pid)] = struct{}{}
			}
		}
	}
	for path, holders := range seen {
		list := make([]int, 0, len(holders))
		for pid := range holders {
			list = append(list, pid)
		}
		sort.Ints(list)
		result[path] = list
	}
	return result, true
}
