
	For Aprisma Developers working on CockroachDB:

	Ensure that you are running on a linux/mac system before running build/builder.sh
	The shell script was specifically configured for these OSes only

	Ensure that you are working on a release branch in order to extend the functions

	I denote types as "type_name" and variables as |var_name|

	the entry point for adding new SQL functions is located in the variable |builtins|,
	which is a map of key: "string" and value: "builtinDefinition"
		- the key represents the SQL function name hence its type is "string"
		- the value is a struct type that is named as "builtinDefinition"

	"builtinDefinition" is a struct type that consists of 2 values:
		- |props|: a variable of type "Tree.FunctionProperties" and is assumed to be some sort of descriptive property of the function?
		   - several are seen so far, e.g. assigning a category or whether it accepts null arguments
		   - some SQL functions use defProp() to create a default empty "Tree.FunctionProperties" 
		- |overload|: a slice of type "tree.Overload" .  it represents a struct of different overload types for different data types
		  pertaining to the SQL function

	
	many functions uses the makeBuiltin() functions, which returns a struct of "BuiltInDefinitions" to the map |builtins|;
	it accepts a "tree.FunctionProperties" and "tree.Overload" argument

	many functions uses various stringOverload1(), stringOverload2() etc; these functions are helper functions
	that return a "tree.Overload" struct that is required by the makeBuiltin(), but the struct has default values such as "string"

	the base "tree.Overload" struct has several members and is located in overload.go ; only the important are listed:
		- types as "TypeList": "TypeList" exists in overload.go and the list of types in types.go; this field represents the list of arguments to the SQL function and is typically specified as 
		  a struct list of type "ArgTypes"
		  	- struct list ArgTypes contains 2 fields as an anonymous struct and is located in overload.go:
		  		- name as "String", and is used to present a human readable string
		  		- typ as "Types.T", the actual type of the argument
		  	- "TypeList" is an interface type and not an actual list or array; 
		- returnType as "ReturnTyper": "ReturnTyper" exist in overload.go and types in types.go; this field represents the return type
		- info as "String"
		- various function types as callables
			- you will need to specify data types in term of datums(singular noun of data); 
			  datum.go has a list of data types

	any function assigned so far always accepts *tree.EvalContext and tree.Datums
		- currently unknown what is *tree.EvalConttext; it is assumed that it is required
		- tree.Datums is located in Datum.go and represents a slice of the values provided as arguments to the SQL functions

	currently, the pailler and elgamal functions return in the octet escape format; further work may be done to revise it into hexadecimal format
	the sql test suite must convert the output using the encode() function to hex in order to obtain a hex string
	 as the text file for logic tests is not conducive for control characters

	for aggregate functions, a similiar scheme is used; the entry point for adding a new SQL aggregate function 
	is located in the variable |aggregates| aggregate_builtins.go that follows the structure of |builtins| in builtins.go

	an SQL aggregate function's "builtInDefiniton" can use the makeBuiltin() function has the following definition, but with restrictions
	  	- |props| : uses either aggProps() or aggPropsNullableArgs() only
	  	- |overload| : use makeAggOverload() for simplicity, but there is makeAggOverloadWithReturnType(); makeAggOverload() calls makeAggOverloadWithReturnType(), which determines and returns a "tree.Overload" struct with configured values for sql aggregation function 

	to define an aggregate function, several steps must be performed in aggregate_builtins.go
		- define a struct type "myAggStruct", with any properties required for your aggregate computation; typically it contains the following
			- a "bool" flag for ensuring that a null value is returned if there are no non-null values in the aggregate(based on analysis of seenNonNull/sawNonNull)
			- a variable to hold a partial sum
			- for certain function, there is a memory constraint to be maintained and monitored, hence it may be allocated with a "mon.BoundAccount" for automatic memory monitoring using makeBoundAccount(), located in pkg/utils/mon/bytes_usage.go

		- declare a variable reference to "myAggStruct", with the variable type as the "tree.AggregateFunc" interface type, located in aggregate_funcs.go
		- declare a constant of the size of the AggregateStruct
		- declare a function that is responsible for making the struct; it takes the following arugments
			(params []types.T, evalCtx *tree.EvalContext, arguments tree.Datums) 
			and should be invoked by makeAggOverload(), which will configure the sqls
			- note that this function represents the point in which the SQL function is invoked, and its 
			  accumulation operations are prepared, but not executed on supplied rows; hence if a fixed argument needs to be received from the SQL input, receive it here
		- define pointer receiver functions on the struct, it must implement the methods declared in the "tree.AggregateFunc" interface type
		- add the function name to the ConstructAggregate() function in groupby.go
		- add the function name to the AggregateOpReverseMap map and the set of functions Aggregate___(op operator) in operator.go
		- add the function name to scalar.opt, using the built-in structure as a reference
		- add the function name to pkg\sql\distsqlpb\processors.proto, using the built-in functions as a reference for the naming convention
		- if you need to receive an additional argument, you also need to add modify pkg/sql/opt/exec/execbuilder/relational_builder.go to force extractAggregateConstArgs() to receive the required arguments; as of time of writing(8/2019) the internal documentation only mentions that it supports constants?

