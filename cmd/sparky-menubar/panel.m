//go:build darwin && cgo

#import "panel.h"
#import <Cocoa/Cocoa.h>

extern void goPanelOpened(void);
extern void goRefresh(void);
extern void goPickerOpen(void);
extern void goPickerClose(void);
extern void goChoiceClick(const char *modelID);
extern void goToggleTitle(void);
extern void goToggleLogin(void);
extern void goCopyDiagnostics(void);
extern void goQuit(void);

static NSStatusItem *gItem;
static NSString *Str(const char *s) { return s ? [NSString stringWithUTF8String:s] : @""; }

// Only informational rows need a view. Actions, checkmarks, submenus, keyboard
// navigation, highlighting and the menu surface are drawn entirely by AppKit.
@interface SparkyInfoRow : NSView
@property (nonatomic, strong) NSTextField *left;
@property (nonatomic, strong) NSTextField *right;
- (instancetype)initSmall:(BOOL)small strong:(BOOL)strong pair:(BOOL)pair;
- (void)setLeft:(NSString *)left right:(NSString *)right;
@end

@implementation SparkyInfoRow
- (instancetype)initSmall:(BOOL)small strong:(BOOL)strong pair:(BOOL)pair {
    if ((self = [super initWithFrame:NSMakeRect(0, 0, 300, small ? 18 : 22)])) {
        self.autoresizingMask = NSViewWidthSizable;
        self.left = [NSTextField labelWithString:@""];
        self.right = [NSTextField labelWithString:@""];
        self.left.font = [NSFont systemFontOfSize:small ? 12 : 13
                                          weight:strong ? NSFontWeightMedium : NSFontWeightRegular];
        self.right.font = pair ? [NSFont monospacedDigitSystemFontOfSize:13 weight:NSFontWeightRegular]
                               : [NSFont systemFontOfSize:12];
        self.left.textColor = small ? NSColor.secondaryLabelColor : NSColor.labelColor;
        self.right.textColor = pair ? NSColor.labelColor : NSColor.secondaryLabelColor;
        self.left.maximumNumberOfLines = 1;
        self.right.maximumNumberOfLines = 1;
        self.left.lineBreakMode = NSLineBreakByTruncatingTail;
        self.right.lineBreakMode = NSLineBreakByClipping;
        self.right.alignment = NSTextAlignmentRight;
        [self addSubview:self.left];
        [self addSubview:self.right];
    }
    return self;
}
- (void)setLeft:(NSString *)left right:(NSString *)right {
    self.left.stringValue = left ?: @"";
    self.right.stringValue = right ?: @"";
    self.toolTip = right.length ? [NSString stringWithFormat:@"%@: %@", left, right] : left;
    self.needsLayout = YES;
}
- (void)layout {
    [super layout];
    CGFloat width = NSWidth(self.bounds), height = NSHeight(self.bounds);
    CGFloat valueWidth = self.right.stringValue.length ? ceil(self.right.intrinsicContentSize.width) : 0;
    self.right.hidden = valueWidth == 0;
    self.left.frame = NSMakeRect(18, floor((height - self.left.intrinsicContentSize.height) / 2),
                                MAX(0, width - 36 - (valueWidth ? valueWidth + 8 : 0)),
                                self.left.intrinsicContentSize.height);
    self.right.frame = NSMakeRect(width - 18 - valueWidth,
                                 floor((height - self.right.intrinsicContentSize.height) / 2),
                                 valueWidth, self.right.intrinsicContentSize.height);
}
@end

@interface SparkyMenuController : NSObject <NSMenuDelegate>
@property (nonatomic, strong) NSMenu *menu;
@property (nonatomic, strong) NSMenu *models;
@property (nonatomic, strong) NSMenu *settings;
@property (nonatomic, strong) NSMenuItem *loadItem;
@property (nonatomic, strong) NSMenuItem *refreshItem;
@property (nonatomic, strong) NSMenuItem *quitItem;
@property (nonatomic, strong) NSMenuItem *loginItem;
@property (nonatomic, strong) NSMenuItem *titleItem;
@property (nonatomic, strong) NSMenuItem *resultItem;
@property (nonatomic, strong) NSMenuItem *catalogItem;
@property (nonatomic, strong) SparkyInfoRow *header;
@property (nonatomic, strong) SparkyInfoRow *model;
@property (nonatomic, strong) SparkyInfoRow *location;
@property (nonatomic, strong) SparkyInfoRow *memoryCaption;
@property (nonatomic, strong) SparkyInfoRow *requests;
@property (nonatomic, strong) SparkyInfoRow *footer;
@property (nonatomic, strong) SparkyInfoRow *result;
@property (nonatomic, strong) SparkyInfoRow *catalogState;
@property (nonatomic, strong) NSArray<SparkyInfoRow *> *extraRows;
@property (nonatomic, strong) NSArray<SparkyInfoRow *> *memoryRows;
@property (nonatomic, strong) NSArray<NSString *> *extras;
@property (nonatomic, strong) NSArray<NSString *> *memoryLabels;
@property (nonatomic, strong) NSArray<NSString *> *memoryValues;
@property (nonatomic, strong) NSArray<NSString *> *choiceLabels;
@property (nonatomic, strong) NSArray<NSString *> *choiceIDs;
@property (nonatomic, strong) NSString *pickerCaption;
@property (nonatomic) BOOL tracking;
@property (nonatomic) BOOL modelTracking;
@property (nonatomic) BOOL mainDirty;
@property (nonatomic) BOOL modelsDirty;
@property (nonatomic) BOOL loadEnabled;
@property (nonatomic) BOOL hasCLI;
- (void)buildMain;
- (void)buildModels;
- (void)performBlock:(dispatch_block_t)block;
@end

static SparkyMenuController *gController;

static NSMenu *NewMenu(NSString *title) {
    NSMenu *menu = [[NSMenu alloc] initWithTitle:title];
    menu.autoenablesItems = NO;
    menu.font = [NSFont menuFontOfSize:0];
    return menu;
}
static NSMenuItem *Action(NSString *title, SEL selector, id target, NSString *key) {
    NSMenuItem *item = [[NSMenuItem alloc] initWithTitle:title action:selector keyEquivalent:key ?: @""];
    item.target = target;
    return item;
}
static NSMenuItem *Info(SparkyInfoRow *row) {
    NSMenuItem *item = [[NSMenuItem alloc] initWithTitle:@"" action:nil keyEquivalent:@""];
    item.view = row;
    return item;
}

@implementation SparkyMenuController
- (instancetype)init {
    if ((self = [super init])) {
        self.menu = NewMenu(@"Sparky");
        self.menu.minimumWidth = 300;
        self.models = NewMenu(@"Load model");
        self.settings = NewMenu(@"Settings");
        self.menu.delegate = self;
        self.models.delegate = self;
        self.extras = @[]; self.memoryLabels = @[]; self.memoryValues = @[];
        self.choiceLabels = @[]; self.choiceIDs = @[]; self.pickerCaption = @"Loading model list…";
        self.header = [[SparkyInfoRow alloc] initSmall:NO strong:YES pair:NO];
        self.model = [[SparkyInfoRow alloc] initSmall:NO strong:YES pair:NO];
        self.location = [[SparkyInfoRow alloc] initSmall:YES strong:NO pair:NO];
        self.memoryCaption = [[SparkyInfoRow alloc] initSmall:YES strong:NO pair:NO];
        self.requests = [[SparkyInfoRow alloc] initSmall:NO strong:NO pair:YES];
        self.footer = [[SparkyInfoRow alloc] initSmall:YES strong:NO pair:NO];
        self.result = [[SparkyInfoRow alloc] initSmall:YES strong:NO pair:NO];
        self.catalogState = [[SparkyInfoRow alloc] initSmall:YES strong:NO pair:NO];
        [self.header setLeft:@"Sparky" right:@"Unknown"];
        [self.model setLeft:@"Waiting for status" right:@""];
        self.loadItem = Action(@"Load model…", NULL, nil, @"");
        self.loadItem.submenu = self.models;
        self.refreshItem = Action(@"Refresh status", @selector(refresh:), self, @"");
        self.quitItem = Action(@"Quit Sparky", @selector(quit:), self, @"q");
        self.loginItem = Action(@"Start at login", @selector(toggleLogin:), self, @"");
        self.titleItem = Action(@"Show title", @selector(toggleTitle:), self, @"");
        [self.settings addItem:self.loginItem];
        [self.settings addItem:self.titleItem];
        [self.settings addItem:NSMenuItem.separatorItem];
        [self.settings addItem:Action(@"Copy diagnostics", @selector(diagnostics:), self, @"")];
        [self buildMain];
        [self buildModels];
    }
    return self;
}
- (void)buildMain {
    [self.menu removeAllItems];
    [self.menu addItem:Info(self.header)];
    [self.menu addItem:Info(self.model)];
    [self.menu addItem:Info(self.location)];
    NSMutableArray *extraRows = [NSMutableArray array];
    for (NSString *line in self.extras) {
        SparkyInfoRow *row = [[SparkyInfoRow alloc] initSmall:YES strong:NO pair:NO];
        [row setLeft:line right:@""]; [extraRows addObject:row]; [self.menu addItem:Info(row)];
    }
    self.extraRows = extraRows;
    [self.menu addItem:NSMenuItem.separatorItem];
    [self.menu addItem:Info(self.memoryCaption)];
    NSMutableArray *memoryRows = [NSMutableArray array];
    for (NSUInteger i = 0; i < self.memoryLabels.count; i++) {
        SparkyInfoRow *row = [[SparkyInfoRow alloc] initSmall:NO strong:NO pair:YES];
        [row setLeft:self.memoryLabels[i] right:self.memoryValues[i]];
        [memoryRows addObject:row]; [self.menu addItem:Info(row)];
    }
    self.memoryRows = memoryRows;
    [self.menu addItem:Info(self.requests)];
    self.resultItem = Info(self.result);
    self.resultItem.hidden = self.result.left.stringValue.length == 0;
    [self.menu addItem:self.resultItem];
    [self.menu addItem:NSMenuItem.separatorItem];
    [self.menu addItem:self.loadItem];
    [self.menu addItem:self.refreshItem];
    [self.menu addItem:NSMenuItem.separatorItem];
    [self.menu addItem:Info(self.footer)];
    NSMenuItem *settings = Action(@"Settings", NULL, nil, @"");
    settings.submenu = self.settings;
    [self.menu addItem:settings];
    [self.menu addItem:self.quitItem];
    self.mainDirty = NO;
}
- (void)buildModels {
    [self.models removeAllItems];
    NSString *caption = [self.pickerCaption isEqualToString:@"Run through the Sparky CLI"]
                        ? @"Select a model" : self.pickerCaption;
    [self.catalogState setLeft:caption right:@""];
    self.catalogItem = Info(self.catalogState);
    self.catalogItem.hidden = self.choiceIDs.count && [self.pickerCaption isEqualToString:@"Run through the Sparky CLI"];
    [self.models addItem:self.catalogItem];
    for (NSUInteger i = 0; i < self.choiceIDs.count; i++) {
        NSMenuItem *item = Action(self.choiceLabels[i], @selector(choose:), self, @"");
        item.representedObject = self.choiceIDs[i];
        item.toolTip = self.choiceIDs[i];
        item.enabled = self.loadEnabled && self.hasCLI;
        [self.models addItem:item];
    }
    self.modelsDirty = NO;
}
- (void)menuNeedsUpdate:(NSMenu *)menu {
    if (menu == self.menu && self.mainDirty) [self buildMain];
    if (menu == self.models && self.modelsDirty) [self buildModels];
}
- (void)menuWillOpen:(NSMenu *)menu {
    if (menu == self.menu) { self.tracking = YES; goPanelOpened(); }
    if (menu == self.models) { self.modelTracking = YES; goPickerOpen(); }
}
- (void)menuDidClose:(NSMenu *)menu {
    if (menu == self.menu) self.tracking = NO;
    if (menu == self.models) self.modelTracking = NO;
    // Do not change menu structure inside menuDidClose:.
    if (menu == self.menu || menu == self.models) {
        dispatch_async(dispatch_get_main_queue(), ^{
            goPickerClose();
            if (!self.tracking && self.mainDirty) [self buildMain];
            if (!self.modelTracking && self.modelsDirty) [self buildModels];
        });
    }
}
- (void)performBlock:(dispatch_block_t)block { block(); }
- (void)refresh:(id)sender { goRefresh(); }
- (void)toggleLogin:(id)sender { goToggleLogin(); }
- (void)toggleTitle:(id)sender { goToggleTitle(); }
- (void)diagnostics:(id)sender { goCopyDiagnostics(); }
- (void)quit:(id)sender { goQuit(); }
- (void)choose:(NSMenuItem *)item {
    NSString *modelID = item.representedObject;
    if (modelID.length) goChoiceClick(modelID.UTF8String);
}
@end

static SparkyMenuController *Controller(void) {
    if (!gController) gController = [[SparkyMenuController alloc] init];
    return gController;
}
static void OnMain(dispatch_block_t block) {
    if (NSThread.isMainThread) block();
    else {
        // Menu tracking uses a separate run-loop mode. Keep status and elapsed
        // load progress updating while the native menu is open.
        [gController performSelectorOnMainThread:@selector(performBlock:) withObject:[block copy]
                                  waitUntilDone:NO modes:@[NSDefaultRunLoopMode, NSEventTrackingRunLoopMode]];
    }
}

void sparky_init(void) {
    if (!NSThread.isMainThread || gItem) return;
    @autoreleasepool {
        [NSApplication sharedApplication];
        [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
        SparkyMenuController *c = Controller();
        gItem = [NSStatusBar.systemStatusBar statusItemWithLength:NSVariableStatusItemLength];
        gItem.menu = c.menu;
        NSMenu *main = NewMenu(@"Main");
        NSMenuItem *application = Action(@"Sparky", NULL, nil, @"");
        application.submenu = NewMenu(@"Sparky");
        [application.submenu addItem:Action(@"Quit Sparky", @selector(quit:), c, @"q")];
        [main addItem:application];
        NSApp.mainMenu = main;
        sparky_set_icon_state(0, NULL);
    }
}
void sparky_run(void) {
    if (NSThread.isMainThread) { @autoreleasepool { [NSApp run]; } }
}
void sparky_quit(void) { OnMain(^{ [Controller().menu cancelTracking]; [NSApp terminate:nil]; }); }
void sparky_show_panel(void) { OnMain(^{ [gItem.button performClick:nil]; }); }
void sparky_hide_panel(void) { OnMain(^{ [Controller().menu cancelTracking]; }); }

void sparky_set_icon_state(int state, const char *title) {
    NSString *text = Str(title);
    OnMain(^{
        NSString *symbol = state == 1 ? @"ellipsis" : state == 2 ? @"exclamationmark.triangle" : @"bolt.fill";
        gItem.button.image = [NSImage imageWithSystemSymbolName:symbol accessibilityDescription:@"Sparky"];
        gItem.button.image.template = YES;
        gItem.button.title = text;
        gItem.button.imagePosition = text.length ? NSImageLeft : NSImageOnly;
    });
}
void sparky_set_panel(const char *headerTitle, const char *headerState,
                      const char *modelCaption, const char *modelName, const char *modelLocation,
                      const char *memoryCaption, const char *refreshLabel, const char *loadLabel,
                      const char *footer, const char *loadResult) {
    NSString *ht=Str(headerTitle), *hs=Str(headerState), *mn=Str(modelName), *ml=Str(modelLocation);
    NSString *mc=Str(memoryCaption), *rl=Str(refreshLabel), *ll=Str(loadLabel), *ft=Str(footer), *lr=Str(loadResult);
    OnMain(^{
        SparkyMenuController *c = Controller();
        [c.header setLeft:ht right:hs]; [c.model setLeft:mn right:@""];
        [c.location setLeft:ml right:@""]; [c.memoryCaption setLeft:mc right:@""];
        [c.footer setLeft:ft right:@""]; [c.result setLeft:lr right:@""];
        c.resultItem.hidden = !lr.length;
        c.loadItem.title = ll; c.refreshItem.title = rl;
    });
}
void sparky_set_extra_models(const char **lines, int count) {
    NSMutableArray *values = [NSMutableArray array];
    for (int i=0; i<count && lines; i++) [values addObject:Str(lines[i])];
    OnMain(^{
        SparkyMenuController *c = Controller();
        if ([c.extras isEqualToArray:values]) return;
        c.extras = values; c.mainDirty = YES;
        if (!c.tracking) [c buildMain];
        else for (NSUInteger i=0; i<MIN(values.count,c.extraRows.count); i++) [c.extraRows[i] setLeft:values[i] right:@""];
    });
}
void sparky_set_memory_rows(const char **labels, const char **values, int count) {
    NSMutableArray *l=[NSMutableArray array], *v=[NSMutableArray array];
    for (int i=0; i<count && labels && values; i++) { [l addObject:Str(labels[i])]; [v addObject:Str(values[i])]; }
    OnMain(^{
        SparkyMenuController *c = Controller();
        c.memoryLabels=l; c.memoryValues=v;
        if (l.count != c.memoryRows.count) { c.mainDirty=YES; if (!c.tracking) [c buildMain]; }
        for (NSUInteger i=0; i<MIN(l.count,c.memoryRows.count); i++) [c.memoryRows[i] setLeft:l[i] right:v[i]];
    });
}
void sparky_set_requests(const char *label, const char *value) {
    NSString *l=Str(label), *v=Str(value);
    OnMain(^{ [Controller().requests setLeft:l right:v]; });
}
void sparky_set_picker(int picking, const char *caption, const char **labels, const char **ids, int count) {
    NSString *cap=Str(caption);
    NSMutableArray *l=[NSMutableArray array], *v=[NSMutableArray array];
    for (int i=0; i<count && labels && ids; i++) { [l addObject:Str(labels[i])]; [v addObject:Str(ids[i])]; }
    OnMain(^{
        SparkyMenuController *c = Controller();
        BOOL changed = ![c.choiceLabels isEqualToArray:l] || ![c.choiceIDs isEqualToArray:v];
        c.pickerCaption=cap; c.choiceLabels=l; c.choiceIDs=v;
        [c.catalogState setLeft:[cap isEqualToString:@"Run through the Sparky CLI"] ? @"Select a model" : cap right:@""];
        c.catalogItem.hidden = v.count && [cap isEqualToString:@"Run through the Sparky CLI"];
        if (changed) { c.modelsDirty=YES; if (!c.modelTracking) [c buildModels]; }
    });
}
void sparky_set_enabled(int loadEnabled, int refreshEnabled, int hasCLI) {
    OnMain(^{
        SparkyMenuController *c = Controller();
        c.loadEnabled=loadEnabled != 0; c.hasCLI=hasCLI != 0;
        c.loadItem.enabled=c.loadEnabled && c.hasCLI;
        c.refreshItem.enabled=refreshEnabled != 0;
        for (NSMenuItem *item in c.models.itemArray)
            if (item.action == @selector(choose:)) item.enabled=c.loadItem.enabled;
    });
}
void sparky_set_settings(int showTitle, int loginEnabled) {
    OnMain(^{
        SparkyMenuController *c = Controller();
        c.titleItem.state=showTitle ? NSControlStateValueOn : NSControlStateValueOff;
        c.loginItem.state=loginEnabled ? NSControlStateValueOn : NSControlStateValueOff;
    });
}

// Native construction tests create no status item, window or application loop.
int sparky_probe_build_panel(void) {
    if (!NSThread.isMainThread) return 2;
    @try { Controller(); return 0; } @catch (NSException *ex) { return 1; }
}
int sparky_probe_choice_count(void) {
    if (!NSThread.isMainThread) return -1;
    int count=0;
    for (NSMenuItem *item in Controller().models.itemArray) if (item.action == @selector(choose:)) count++;
    return count;
}
char *sparky_probe_picker_caption(void) { return strdup(Controller().pickerCaption.UTF8String); }
char *sparky_probe_layout(int picking) {
    if (!NSThread.isMainThread) return strdup("native menu test requires main thread");
    SparkyMenuController *c = Controller();
    if (c.loadItem.view || c.refreshItem.view || c.quitItem.view || c.loadItem.submenu != c.models)
        return strdup("actions must use native menu items and a native model submenu");
    if (c.menu.autoenablesItems || c.models.autoenablesItems)
        return strdup("native validation must preserve Go enablement");
    for (SparkyInfoRow *row in @[c.header,c.model,c.location,c.memoryCaption,c.requests,c.footer]) {
        [row layoutSubtreeIfNeeded];
        if (NSHeight(row.frame)>22 || NSWidth(row.frame)<300 || row.left.maximumNumberOfLines>1)
            return strdup("information rows must use compact single-line menu geometry");
    }
    for (NSMenuItem *item in c.models.itemArray) {
        if (item.action == @selector(choose:) && (!item.representedObject || item.view || item.target != c))
            return strdup("model choices must keep native selection and canonical IDs");
    }
    // Verify the run-loop mode used by an open menu, without creating a window.
    __block BOOL updated = NO;
    dispatch_semaphore_t queued = dispatch_semaphore_create(0);
    dispatch_async(dispatch_get_global_queue(QOS_CLASS_DEFAULT, 0), ^{
        OnMain(^{ updated = YES; });
        dispatch_semaphore_signal(queued);
    });
    if (dispatch_semaphore_wait(queued, dispatch_time(DISPATCH_TIME_NOW, NSEC_PER_SEC)))
        return strdup("background menu update was not queued");
    CFRunLoopRunInMode((__bridge CFStringRef)NSEventTrackingRunLoopMode, 0.03, false);
    if (!updated) return strdup("status updates stalled in native menu tracking mode");
    return NULL;
}
