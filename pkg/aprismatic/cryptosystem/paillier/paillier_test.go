/*
temporary package for testing
based on golang documentation, the file can be changed to paillier_test to include it into the integerated test suite
provided we can figure out whether cockroachdb has messed around with the test in any way
*/
package paillier

import (
	//official, internal, test suite
	"testing"
	//external, officai augmented testing suite; may require installation of external dependencies
	"gotest.tools/assert"
)

func TestAddition(t *testing.T) {

	first := []byte{1,2}
	second := []byte{3,4}
	NSquare := []byte{10}
	expected := []byte{3,8}

	res := Add(first, second, NSquare)

	assert.DeepEqual(t, expected, res)
}

func TestSubtraction(t *testing.T) {

	first := []byte{1,2}
	second := []byte{3,4}
	NSquare := []byte{10}
	expected := []byte{4,6}

	res := Subtract(first, second, NSquare)

	assert.DeepEqual(t, expected, res)
}