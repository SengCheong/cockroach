package paillier

import (
	"math/big"
)

func Add(first []byte, second []byte, NSquare []byte) (result []byte) {

	
	firstActual := make([]byte, len(first)/2)
	copy(firstActual, first[:len(first)/2])
	firstNegative := make([]byte, len(first)/2)
	copy(firstNegative, first[len(first)/2:])
	
	secondActual := make([]byte, len(second)/2)
	copy(secondActual, second[:len(second)/2])
	secondNegative := make([]byte, len(second)/2)
	copy(secondNegative, second[len(second)/2:])
	
	addActual := addParts(firstActual, secondActual, NSquare)
	addNegative := addParts(firstNegative, secondNegative, NSquare)
	
	result = make([]byte, len(first))
	copy(result, addActual[:len(addActual)])
	copy(result[len(result)/2:], addNegative) 
	
	return result
}

func Subtract(first []byte, second []byte, NSquare []byte) (result []byte) {

	
	firstActual := make([]byte, len(first)/2)
	copy(firstActual, first[:len(first)/2])
	firstNegative := make([]byte, len(first)/2)
	copy(firstNegative, first[len(first)/2:])
	
	secondActual := make([]byte, len(second)/2)
	copy(secondActual, second[:len(second)/2])
	secondNegative := make([]byte, len(second)/2)
	copy(secondNegative, second[len(second)/2:])
	
	subActual := addParts(firstActual, secondNegative, NSquare)
	subNegative := addParts(firstNegative, secondActual, NSquare)
	
	result = make([]byte, len(first))
	copy(result, subActual[:len(subActual)])
	copy(result[len(result)/2:], subNegative) 
	
	return result
}


//only function names that are capitalized will be exported this function is private so it is not capitalized
func addParts(first []byte, second []byte, NSquare []byte) (result []byte) {
	
	A := new(big.Int)
	A.SetBytes(first)
	
	B := new(big.Int)
	B.SetBytes(second)
	
	NSquareBig := new(big.Int)
	NSquareBig.SetBytes(NSquare)
	
	WorkVar := new(big.Int)
	WorkVar.Mul(A,B)
	WorkVar.Mod(WorkVar, NSquareBig)
	
	result = WorkVar.Bytes()
	
	return result
}