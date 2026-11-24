package startup

import (
	"fmt"
	"os"
)

func privateFile(file string) (os.FileInfo, error) {
	info, err := os.Stat(file)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("認証ファイルは所有者だけが読める通常ファイルにしてください")
	}
	return info, nil
}
