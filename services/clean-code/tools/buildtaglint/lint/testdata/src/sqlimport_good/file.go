// Imports only safe stdlib packages. The analyzer MUST NOT
// fire.
package sqlimport_good

import (
	"fmt"
	"strings"
)

func Hello(name string) string {
	return fmt.Sprintf("hello, %s", strings.ToUpper(name))
}
