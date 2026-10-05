package approver

import (
	"net/http"

	"github.com/dotpopo/mcp-lane/shared/approverpage"
)

// PageRoutes mounts the device page next to the API it calls. The page
// itself lives in shared/approverpage so that a relay can serve the same
// one; only the API below has to reach this particular Companion.
func (s *Service) PageRoutes(mux *http.ServeMux) {
	approverpage.Routes(mux)
}
