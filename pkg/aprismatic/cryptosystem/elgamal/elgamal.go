package elgamal

import (
	"math/big"
)

func Multiply(first []byte, second []byte, P []byte) (result []byte) {

	firstNumerator := make([]byte, len(first)/2)
	copy(firstNumerator, first[:len(first)/2])
	firstDenominator := make([]byte, len(first)/2)
	copy(firstDenominator, first[len(first)/2:])
	
	secondNumerator := make([]byte, len(second)/2)
	copy(secondNumerator, second[:len(second)/2])
	secondDenominator := make([]byte, len(second)/2)
	copy(secondDenominator, second[len(second)/2:])
	
	mulNumerator := multiplyParts(firstNumerator, secondNumerator, P)
	mulDenominator := multiplyParts(firstDenominator, secondDenominator, P)

	result = make([]byte, len(first))
	copy(result, mulNumerator[:len(mulNumerator)])
	copy(result[len(result)/2:], mulDenominator) 

	return result

}

func Divide(first []byte, second []byte, P []byte) (result []byte) {

	firstNumerator := make([]byte, len(first)/2)
	copy(firstNumerator, first[:len(first)/2])
	firstDenominator := make([]byte, len(first)/2)
	copy(firstDenominator, first[len(first)/2:])
	
	secondNumerator := make([]byte, len(second)/2)
	copy(secondNumerator, second[:len(second)/2])
	secondDenominator := make([]byte, len(second)/2)
	copy(secondDenominator, second[len(second)/2:])
	
	mulNumerator := multiplyParts(firstNumerator, secondDenominator, P)
	mulDenominator := multiplyParts(firstDenominator, secondNumerator, P)

	result = make([]byte, len(first))
	copy(result, mulNumerator[:len(mulNumerator)])
	copy(result[len(result)/2:], mulDenominator) 

	return result
}

func multiplyParts(first []byte, second []byte, P []byte) (result []byte) {

	blocksize := len(first)

	temp := make([]byte, blocksize/2)
	
	copy(temp, first[:blocksize/2])
	A_left := new(big.Int)
	A_left.SetBytes(temp)

	copy(temp, first[blocksize/2:])
	A_right := new(big.Int)
	A_right.SetBytes(temp)

	copy(temp, second[:blocksize/2])
	B_left := new(big.Int)
	B_left.SetBytes(temp)

	copy(temp, second[blocksize/2:])
	B_right := new(big.Int)
	B_right.SetBytes(temp)

	P_big := new(big.Int)
	P_big.SetBytes(P)

	compute_left := new(big.Int)
	compute_left.Mul(A_left, B_left)
	compute_left.Mod(compute_left, P_big)

	compute_right := new(big.Int)
	compute_right.Mul(A_right, B_right)
	compute_right.Mod(compute_right, P_big)

	result = make([]byte, blocksize)
	copy(result, compute_left.Bytes())
	copy(result[blocksize/2:], compute_right.Bytes())

	return result
}