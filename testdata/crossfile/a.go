package crossfile

// Adjust converts a raw reading into a calibrated one.
func Adjust(raw int) int {
	return raw + Offset
}
