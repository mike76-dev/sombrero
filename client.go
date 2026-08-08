package main

// smbClient is a Client object: what the server remembers about a client between the connections
// it dials, keyed in the global client table by the GUID the client names itself with. The dialect
// is the client's to choose once, and holding it here is what lets a later connection be held to
// the same one ([MS-SMB2] 3.3.5.5.3).
type smbClient struct {
	dialect uint16
}
