package procchild

// GroupKind names how a child was isolated from its supervisor, for the
// record and for the stop.
//
// fleet-runtime.md §13 P4 requires the child to start "in its own process
// group (systemd --user scope on Linux, Job object on Windows, process group
// on macOS)". The reason is not tidiness: a child sharing the supervisor's
// group shares every signal sent to that group, so a stop aimed at the child
// would also kill the supervisor — and a supervisor killed mid-stop writes no
// settle, which the daemon then correctly reads as `lost`. The group is what
// lets a graceful stop reach the whole tree and nothing else.
type GroupKind string

const (
	// GroupProcessGroup is a POSIX process group of its own (setpgid).
	GroupProcessGroup GroupKind = "process_group"
	// GroupSystemdScope is a transient `systemd --user` scope: its own cgroup,
	// so a stop reaches descendants that changed their own process group.
	GroupSystemdScope GroupKind = "systemd_scope"
	// GroupJobObject is a Windows Job object with kill-on-close.
	GroupJobObject GroupKind = "job_object"
	// GroupNone is a platform with no isolation primitive at all. It is
	// recorded rather than hidden: a stop on such a host reaches one pid, and
	// the record has to say so instead of implying a tree.
	GroupNone GroupKind = "none"
)
