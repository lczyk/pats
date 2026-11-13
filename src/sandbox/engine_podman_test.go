//go:build !nopodman

package sandbox

// podman's rows of the cross-driver tables, so a build without podman drops
// them with it.
func init() {
	conformDrivers = append(conformDrivers, containerConform("podman"))
	driverCases = append(driverCases, containerDriverCase("podman"))
}
