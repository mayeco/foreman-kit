package gitx

import "context"

// SubmoduleUpdateInit runs "git submodule update --init <args>" in dir with the file transport allowed
// for that command alone. Since git 2.38.1 a submodule whose URL is a local path (a local bare remote,
// as in the simulator and the real-test beds) is refused with "transport 'file' not allowed" unless
// protocol.file.allow=always is in effect. The option goes on the command line, never into a git
// configuration file, so no other git command of the kit or of an agent is affected.
func SubmoduleUpdateInit(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-c", "protocol.file.allow=always", "submodule", "update", "--init"}, args...)
	return RunGit(ctx, dir, full...)
}
