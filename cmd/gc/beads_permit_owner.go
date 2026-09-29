package main

var hostBeadsPermitAuthorityOwnerIsTrusted = hostBeadsPermitOwnerIsRoot
var hostBeadsPermitBrokerOwnerIsTrusted = hostBeadsPermitOwnerIsRoot
var hostBeadsPermitBrokerUIDIsTrusted = hostBeadsPermitOwnerUIDIsRoot

func hostBeadsPermitOwnerUIDIsTrusted(ownerUID uint64, effectiveUID int) bool {
	return ownerUID == 0 || (effectiveUID >= 0 && ownerUID == uint64(effectiveUID))
}

func hostBeadsPermitOwnerUIDIsRoot(ownerUID uint64) bool {
	return ownerUID == 0
}
