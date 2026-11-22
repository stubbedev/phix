package unit

import (
	"fmt"
	"strconv"
)

type UMask uint16

func ParseUMask(s string) (UMask, error) {
	m, err := strconv.ParseUint(s, 8, 9)
	if err != nil {
		return 0, fmt.Errorf("umask %q is not an octal mask in 0..0777: %w", s, err)
	}
	return UMask(m), nil
}

func (m UMask) Int() int { return int(m) }

func (m UMask) String() string { return strconv.FormatUint(uint64(m), 8) }
