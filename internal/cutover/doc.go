// Package cutover implements switching the live service onto the migrated instance: binary swap, service definition, restart, quota recalculation.
// See ARCHITECTURE.md §4.5 for the design.
package cutover
