package build

var CurrentCommit string

const BuildVersion = "1.5.1-rc2"

func UserVersion() string {
	return BuildVersion + CurrentCommit
}
