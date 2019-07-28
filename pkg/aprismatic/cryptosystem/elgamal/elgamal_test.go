package elgamal

import (
	//official, internal, test suite
	"testing"
	//external, officai augmented testing suite; may require installation of external dependencies
	"gotest.tools/assert"
)

func TestMul(t *testing.T) {

	first := []byte{1,2,3,4}
	second := []byte{5,6,7,8}
	P := []byte{10}
	expected := []byte{5,2,1,2}

	res := Multiply(first, second, P)

	assert.DeepEqual(t, expected, res)
}

func TestDiv(t *testing.T) {

	first := []byte{1,2,3,4}
	second := []byte{5,6,7,8}
	P := []byte{10}
	expected := []byte{7,6,5,4}

	res := Divide(first, second, P)

	assert.DeepEqual(t, expected, res)
}