package engine

// 小工具：避免为三处用到的少量函数引入额外包。

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
)

type sync_Mutex = sync.Mutex

func itoa(n int) string { return strconv.Itoa(n) }

func errorsNew(s string) error { return errors.New(s) }

func fmtSscan(s string, n *int) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	*n = v
	return len(s), nil
}

func printf(format string, a ...interface{}) { fmt.Printf(format, a...) }
