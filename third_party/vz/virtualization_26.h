//
//  virtualization_26.h
//
//  Created for gocracker local vmnet extensions.
//

#pragma once

#import "virtualization_helper.h"
#import <Virtualization/Virtualization.h>
#import <vmnet/vmnet.h>

/* macOS 26 API */
void *newVZVmnetNetworkDeviceAttachment(void *network);
