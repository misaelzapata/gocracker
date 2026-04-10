//
//  virtualization_26.m
//
//  Created for gocracker local vmnet extensions.
//

#import "virtualization_26.h"

void *newVZVmnetNetworkDeviceAttachment(void *network)
{
#ifdef INCLUDE_TARGET_OSX_26
    if (@available(macOS 26, *)) {
        return [[VZVmnetNetworkDeviceAttachment alloc] initWithNetwork:(vmnet_network_ref)network];
    }
#endif

    RAISE_UNSUPPORTED_MACOS_EXCEPTION();
}
