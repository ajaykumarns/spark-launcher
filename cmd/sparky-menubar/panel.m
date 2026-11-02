//go:build darwin && cgo

#import "panel.h"
#import <Cocoa/Cocoa.h>

// Go-owned callbacks, implemented in callbacks_darwin.go.
extern void goPanelOpened(void);
extern void goRefresh(void);
extern void goPickerOpen(void);
extern void goPickerClose(void);
extern void goChoiceClick(const char *modelID);
extern void goToggleTitle(void);
extern void goToggleLogin(void);
extern void goCopyDiagnostics(void);
extern void goQuit(void);

// The AppKit objects below are only ever touched on the main thread. Go calls
// into this file from arbitrary goroutines; every sparky_* entry point copies
// its string inputs before dispatching, so no Go memory is read off-thread.

static NSStatusItem *gItem = nil;
static NSPopover *gPopover = nil;
static NSViewController *gController = nil;
static BOOL gShowTitle = NO;
static BOOL gLoginEnabled = NO;

static const CGFloat kPanelWidth = 360;
static const CGFloat kInsetX = 14;
static const CGFloat kInsetTop = 12;
static const CGFloat kInsetBottom = 10;
static const CGFloat kInnerX = 10; // shared text inset for headings, sections and rows

// MARK: - Small styled views

static NSTextField *MakeLabel(NSString *text, NSFont *font, NSColor *color) {
    NSTextField *f = [NSTextField labelWithString:text ?: @""];
    f.font = font;
    f.textColor = color;
    f.lineBreakMode = NSLineBreakByTruncatingTail;
    f.maximumNumberOfLines = 1;
    f.focusRingType = NSFocusRingTypeNone;
    f.translatesAutoresizingMaskIntoConstraints = NO;
    [f setContentCompressionResistancePriority:NSLayoutPriorityDefaultLow
                                forOrientation:NSLayoutConstraintOrientationHorizontal];
    return f;
}

static NSTextField *MakeCaption(NSString *text) {
    return MakeLabel(text, [NSFont systemFontOfSize:12 weight:NSFontWeightRegular],
                     [NSColor secondaryLabelColor]);
}

static NSTextField *MakeStrong(NSString *text, CGFloat size) {
    return MakeLabel(text, [NSFont systemFontOfSize:size weight:NSFontWeightMedium],
                     [NSColor labelColor]);
}

static NSTextField *MakeBody(NSString *text) {
    NSTextField *f = MakeLabel(text, [NSFont systemFontOfSize:13 weight:NSFontWeightRegular],
                               [NSColor secondaryLabelColor]);
    f.lineBreakMode = NSLineBreakByWordWrapping;
    f.maximumNumberOfLines = 0;
    return f;
}

static NSTextField *MakeValueLabel(NSString *text, CGFloat size, BOOL mono) {
    NSFont *font = mono
        ? [NSFont monospacedDigitSystemFontOfSize:size weight:NSFontWeightRegular]
        : [NSFont systemFontOfSize:size weight:NSFontWeightRegular];
    NSTextField *f = MakeLabel(text, font, [NSColor labelColor]);
    f.alignment = NSTextAlignmentRight;
    f.lineBreakMode = NSLineBreakByClipping;
    return f;
}

// A bordered=NO text button (used for model choice rows and the gear). Defaults
// to text only (an NSImageOnly default would silently blank buttons that have
// no image). Targets are set explicitly by each call site.
static NSButton *MakeRowButton(NSString *title, SEL action) {
    NSButton *b = [NSButton buttonWithTitle:title ?: @"" target:nil action:action];
    b.bezelStyle = NSBezelStyleRecessed;
    b.bordered = NO;
    b.imagePosition = NSNoImage;
    b.translatesAutoresizingMaskIntoConstraints = NO;
    b.font = [NSFont systemFontOfSize:13 weight:NSFontWeightRegular];
    b.alignment = NSTextAlignmentLeft;
    b.focusRingType = NSFocusRingTypeExterior;
    b.contentTintColor = [NSColor labelColor];
    b.lineBreakMode = NSLineBreakByTruncatingTail;
    b.toolTip = title;
    return b;
}

// A left/right pair row with tabular digits on the value side. The left label
// takes the remaining width; the value keeps its intrinsic size.
static NSStackView *MakePairRow(NSString *left, NSString *right, NSTextField **rightField) {
    NSStackView *row = [NSStackView stackViewWithViews:@[]];
    row.orientation = NSUserInterfaceLayoutOrientationHorizontal;
    row.alignment = NSLayoutAttributeBaseline;
    row.distribution = NSStackViewDistributionFill;
    row.spacing = 8;
    row.translatesAutoresizingMaskIntoConstraints = NO;
    NSTextField *l = MakeLabel(left, [NSFont systemFontOfSize:13 weight:NSFontWeightRegular],
                               [NSColor labelColor]);
    l.maximumNumberOfLines = 1;
    [l setContentHuggingPriority:NSLayoutPriorityDefaultLow
                  forOrientation:NSLayoutConstraintOrientationHorizontal];
    NSTextField *r = MakeValueLabel(right, 13, YES);
    if (rightField) *rightField = r;
    [r setContentHuggingPriority:NSLayoutPriorityRequired
                  forOrientation:NSLayoutConstraintOrientationHorizontal];
    [r setContentCompressionResistancePriority:NSLayoutPriorityRequired
                                forOrientation:NSLayoutConstraintOrientationHorizontal];
    [row addArrangedSubview:l];
    [row addArrangedSubview:r];
    return row;
}

static const CGFloat kGlyphSize = 14;

// Render an SF Symbol as a template image drawn at exactly size×size, so row
// glyphs never inherit the unbounded natural symbol size.
static NSImage *SizeTemplateImage(NSString *symbol, CGFloat size) {
    NSImage *base = [NSImage imageWithSystemSymbolName:symbol
                              accessibilityDescription:nil];
    if (!base) return nil;
    base.template = YES;
    NSImage *img = [[NSImage alloc] initWithSize:NSMakeSize(size, size)];
    [img lockFocus];
    [base drawInRect:NSMakeRect(0, 0, size, size) fromRect:NSZeroRect
             operation:NSCompositingOperationSourceOver fraction:1.0
        respectFlipped:YES hints:nil];
    [img unlockFocus];
    img.template = YES;
    return img;
}

// MARK: - Action row

// SparkyActionRow is a full-width NSButton that draws its row content exactly
// once: an optional leading SF Symbol, its own title cell, and an optional
// trailing SF Symbol pushed to the far edge. The button title remains the cell's
// title (no second custom label, no duplicate drawing), so setTitle: and the
// accessibility label always match what is drawn, and the whole row is one
// click/focus target. The glyphs are decorative images, not subviews that can
// swallow mouse clicks.
@interface SparkyActionRow : NSButton
@property (nonatomic, strong) NSImage *leadingGlyph;
@property (nonatomic, strong) NSImage *trailingGlyph;
@property (nonatomic, assign) BOOL leadingGlyphFirst;
+ (instancetype)rowWithTitle:(NSString *)title glyph:(NSString *)symbol
                      target:(id)target action:(SEL)action glyphLeading:(BOOL)leading;
@end

@implementation SparkyActionRow
+ (instancetype)rowWithTitle:(NSString *)title glyph:(NSString *)symbol
                      target:(id)target action:(SEL)action glyphLeading:(BOOL)leading {
    SparkyActionRow *b = [SparkyActionRow buttonWithTitle:title ?: @"" target:target
                                                   action:action];
    b.bezelStyle = NSBezelStyleRecessed;
    b.bordered = NO;
    b.translatesAutoresizingMaskIntoConstraints = NO;
    b.font = [NSFont systemFontOfSize:13 weight:NSFontWeightRegular];
    b.alignment = NSTextAlignmentLeft;
    b.focusRingType = NSFocusRingTypeExterior;
    // Text stays primary label color; glyphs are template images so AppKit tints
    // them with the current text color.
    b.contentTintColor = [NSColor labelColor];
    // Both symbols are rendered into a fixed 14pt template image so they draw at
    // a restrained size instead of their unbounded natural symbol size.
    NSImage *img = symbol ? SizeTemplateImage(symbol, kGlyphSize) : nil;
    b.leadingGlyphFirst = leading;
    if (leading) {
        // Leading symbols use the button's real image slot beside its title cell;
        // this is what actually draws the Back chevron.
        b.leadingGlyph = img;
        b.image = img;
        b.imagePosition = NSImageLeading;
    } else {
        // Trailing symbols are drawn in drawRect: at the far right edge, matching
        // the approved reference; no second title view is involved.
        b.trailingGlyph = img;
    }
    [b.heightAnchor constraintGreaterThanOrEqualToConstant:28].active = YES;
    return b;
}

// A trailing glyph is drawn at the far edge over the button's own background;
// NSButton still handles hit-testing and keyboard focus for the whole row.
- (void)drawRect:(NSRect)dirtyRect {
    [super drawRect:dirtyRect];
    if (self.trailingGlyph) {
        NSRect bounds = [self alignmentRectForFrame:self.bounds];
        CGFloat w = MIN(kGlyphSize, NSWidth(bounds));
        CGFloat h = MIN(kGlyphSize, NSHeight(bounds));
        NSRect r = NSMakeRect(NSMaxX(bounds) - w, NSMidY(bounds) - h / 2, w, h);
        [self.trailingGlyph drawInRect:r fromRect:NSZeroRect
                             operation:NSCompositingOperationSourceOver
                              fraction:1.0 respectFlipped:YES hints:nil];
    }
}
@end

static SparkyActionRow *MakeActionRow(NSString *title, NSString *symbol, id target,
                                      SEL action, BOOL leading) {
    return [SparkyActionRow rowWithTitle:title glyph:symbol target:target
                                  action:action glyphLeading:leading];
}

static NSBox *MakeDivider(void) {
    NSBox *box = [[NSBox alloc] init];
    box.boxType = NSBoxSeparator;
    box.translatesAutoresizingMaskIntoConstraints = NO;
    return box;
}

// Inset the whole control, keeping native text drawing and hit testing intact.
static void AddInsetRow(NSStackView *parent, NSView *view) {
    NSStackView *row = [NSStackView stackViewWithViews:@[view]];
    row.orientation = NSUserInterfaceLayoutOrientationVertical;
    row.alignment = NSLayoutAttributeLeading;
    row.edgeInsets = NSEdgeInsetsMake(0, kInnerX, 0, kInnerX);
    row.detachesHiddenViews = YES;
    [parent addArrangedSubview:row];
    [row.widthAnchor constraintEqualToAnchor:parent.widthAnchor].active = YES;
    [view.widthAnchor constraintEqualToAnchor:row.widthAnchor constant:-2 * kInnerX].active = YES;
}

// MARK: - Panel controller

@interface SparkyPanelController : NSViewController
@property (nonatomic, strong) NSTextField *headerTitle;
@property (nonatomic, strong) NSTextField *headerState;
@property (nonatomic, strong) NSTextField *modelCaption;
@property (nonatomic, strong) NSTextField *modelName;
@property (nonatomic, strong) NSTextField *modelLocation;
@property (nonatomic, strong) NSStackView *extraModelsStack;
@property (nonatomic, strong) NSTextField *memoryCaption;
@property (nonatomic, strong) NSStackView *memoryStack;
@property (nonatomic, strong) NSStackView *requestsRow;
@property (nonatomic, strong) NSBox *divider1;
@property (nonatomic, strong) NSBox *divider2;
@property (nonatomic, strong) SparkyActionRow *loadButton;
@property (nonatomic, strong) SparkyActionRow *refreshButton;
@property (nonatomic, strong) NSButton *settingsButton;
@property (nonatomic, strong) NSTextField *loadResult;
@property (nonatomic, strong) NSTextField *footer;

// Overview group and picker group are siblings inside the content stack; only
// one is arranged/hidden at a time, so hidden content reserves no space.
@property (nonatomic, strong) NSStackView *overviewGroup;
@property (nonatomic, strong) NSStackView *pickerGroup;
@property (nonatomic, strong) NSTextField *pickerCaption;
@property (nonatomic, strong) NSStackView *choiceStack;
@property (nonatomic, strong) NSView *contentView;
- (void)buildView;
- (void)applyPicking:(BOOL)picking;
- (void)refreshStatus:(id)sender;
- (void)toggleLoad:(id)sender;
- (void)openSettings:(id)sender;
- (void)backToOverview:(id)sender;
- (void)choiceClicked:(id)sender;
- (void)togglePopover:(id)sender;
- (void)toggleLogin:(id)sender;
- (void)toggleTitle:(id)sender;
- (void)copyDiagnostics:(id)sender;
- (void)quitApp:(id)sender;
@end

@implementation SparkyPanelController

- (void)loadView {
    self.view = [[NSView alloc] initWithFrame:NSMakeRect(0, 0, kPanelWidth, 400)];
    [self buildView];
}

- (void)buildView {
    NSView *root = self.view;

    // Content stack: pinned to the root edges, with insets expressed via a
    // guide so children can fill the *inset* width instead of overflowing it.
    NSStackView *content = [NSStackView stackViewWithViews:@[]];
    content.orientation = NSUserInterfaceLayoutOrientationVertical;
    content.alignment = NSLayoutAttributeLeading;
    content.spacing = 0;
    content.translatesAutoresizingMaskIntoConstraints = NO;
    [root addSubview:content];
    self.contentView = content;

    NSLayoutGuide *guide = [[NSLayoutGuide alloc] init];
    [root addLayoutGuide:guide];
    [NSLayoutConstraint activateConstraints:@[
        [content.leadingAnchor constraintEqualToAnchor:guide.leadingAnchor],
        [content.trailingAnchor constraintEqualToAnchor:guide.trailingAnchor],
        [content.topAnchor constraintEqualToAnchor:guide.topAnchor],
        [content.bottomAnchor constraintEqualToAnchor:guide.bottomAnchor],
        [guide.leadingAnchor constraintEqualToAnchor:root.leadingAnchor constant:kInsetX],
        [guide.trailingAnchor constraintEqualToAnchor:root.trailingAnchor constant:-kInsetX],
        [guide.topAnchor constraintEqualToAnchor:root.topAnchor constant:kInsetTop],
        [guide.bottomAnchor constraintEqualToAnchor:root.bottomAnchor constant:-kInsetBottom],
        [root.widthAnchor constraintEqualToConstant:kPanelWidth],
    ]];
    // Children that should span the content width.
    void (^span)(NSView *) = ^(NSView *v) {
        [v.widthAnchor constraintEqualToAnchor:content.widthAnchor].active = YES;
    };

    // Header: title left, state right.
    NSStackView *headerRow = [NSStackView stackViewWithViews:@[]];
    headerRow.orientation = NSUserInterfaceLayoutOrientationHorizontal;
    headerRow.distribution = NSStackViewDistributionFill;
    headerRow.spacing = 8;
    headerRow.edgeInsets = NSEdgeInsetsMake(8, kInnerX, 8, kInnerX);
    self.headerTitle = MakeStrong(@"Sparky", 13);
    self.headerState = MakeLabel(@"Ready", [NSFont systemFontOfSize:13 weight:NSFontWeightRegular],
                                 [NSColor secondaryLabelColor]);
    self.headerState.alignment = NSTextAlignmentRight;
    NSView *headerSpace = [[NSView alloc] initWithFrame:NSZeroRect];
    headerSpace.translatesAutoresizingMaskIntoConstraints = NO;
    [headerRow addArrangedSubview:self.headerTitle];
    [headerRow addArrangedSubview:headerSpace];
    [headerRow addArrangedSubview:self.headerState];
    [content addArrangedSubview:headerRow];
    span(headerRow);

    // Overview group: everything shown by default.
    self.overviewGroup = [NSStackView stackViewWithViews:@[]];
    self.overviewGroup.orientation = NSUserInterfaceLayoutOrientationVertical;
    self.overviewGroup.alignment = NSLayoutAttributeLeading;
    self.overviewGroup.spacing = 0;
    [content addArrangedSubview:self.overviewGroup];
    span(self.overviewGroup);

    // Language model section.
    self.modelCaption = MakeCaption(@"Language model");
    self.modelName = MakeStrong(@"Unknown", 13);
    self.modelName.lineBreakMode = NSLineBreakByWordWrapping;
    self.modelName.maximumNumberOfLines = 0;
    self.modelLocation = MakeBody(@"Waiting for status");
    self.extraModelsStack = [NSStackView stackViewWithViews:@[]];
    self.extraModelsStack.orientation = NSUserInterfaceLayoutOrientationVertical;
    self.extraModelsStack.alignment = NSLayoutAttributeLeading;
    self.extraModelsStack.spacing = 2;

    NSStackView *modelSection = [NSStackView stackViewWithViews:@[
        self.modelCaption, self.modelName, self.modelLocation, self.extraModelsStack
    ]];
    modelSection.orientation = NSUserInterfaceLayoutOrientationVertical;
    modelSection.alignment = NSLayoutAttributeLeading;
    modelSection.spacing = 3;
    modelSection.edgeInsets = NSEdgeInsetsMake(8, kInnerX, 8, kInnerX);
    [self.overviewGroup addArrangedSubview:modelSection];
    span(modelSection);
    for (NSView *v in @[self.modelName, self.modelLocation, self.extraModelsStack]) {
        [v.widthAnchor constraintEqualToAnchor:modelSection.widthAnchor
                                      constant:-2 * kInnerX].active = YES;
    }

    // Wrapping text is constrained to the section's inner width so long model
    // names and errors wrap inside the panel rather than widening it.
    self.modelName.preferredMaxLayoutWidth = kPanelWidth - 2 * kInsetX - 2 * kInnerX;
    self.modelLocation.preferredMaxLayoutWidth = kPanelWidth - 2 * kInsetX - 2 * kInnerX;
    [self.modelName setContentCompressionResistancePriority:NSLayoutPriorityDefaultLow
                                             forOrientation:NSLayoutConstraintOrientationHorizontal];

    self.divider1 = MakeDivider();
    [self.overviewGroup addArrangedSubview:self.divider1];
    span(self.divider1);

    // Memory section.
    self.memoryCaption = MakeCaption(@"Device memory · in use / total");
    self.memoryStack = [NSStackView stackViewWithViews:@[]];
    self.memoryStack.orientation = NSUserInterfaceLayoutOrientationVertical;
    self.memoryStack.alignment = NSLayoutAttributeLeading;
    self.memoryStack.spacing = 4;

    NSTextField *requestsValueTmp = nil;
    self.requestsRow = MakePairRow(@"Active requests", @"0", &requestsValueTmp);

    NSStackView *memorySection = [NSStackView stackViewWithViews:@[
        self.memoryCaption, self.memoryStack, self.requestsRow
    ]];
    memorySection.orientation = NSUserInterfaceLayoutOrientationVertical;
    memorySection.alignment = NSLayoutAttributeLeading;
    memorySection.spacing = 6;
    memorySection.edgeInsets = NSEdgeInsetsMake(8, kInnerX, 8, kInnerX);
    [self.overviewGroup addArrangedSubview:memorySection];
    span(memorySection);
    [self.memoryStack.widthAnchor constraintEqualToAnchor:memorySection.widthAnchor
                                                  constant:-2 * kInnerX].active = YES;
    [self.requestsRow.widthAnchor constraintEqualToAnchor:memorySection.widthAnchor
                                                  constant:-2 * kInnerX].active = YES;

    self.divider2 = MakeDivider();
    [self.overviewGroup addArrangedSubview:self.divider2];
    span(self.divider2);

    // Load result line: short, only visible when the last load needs attention.
    self.loadResult = MakeLabel(@"", [NSFont systemFontOfSize:12 weight:NSFontWeightRegular],
                                [NSColor secondaryLabelColor]);
    self.loadResult.maximumNumberOfLines = 0;
    self.loadResult.lineBreakMode = NSLineBreakByWordWrapping;
    self.loadResult.preferredMaxLayoutWidth = kPanelWidth - 2 * kInsetX - 2 * kInnerX;
    AddInsetRow(self.overviewGroup, self.loadResult);

    // Actions: Load row with trailing chevron, then Refresh with trailing icon.
    self.loadButton = MakeActionRow(@"Load model…", @"chevron.right", self,
                                    @selector(toggleLoad:), NO);
    AddInsetRow(self.overviewGroup, self.loadButton);

    // Refresh keeps its icon at the far right to match the approved reference,
    // like the Load row's trailing chevron.
    self.refreshButton = MakeActionRow(@"Refresh status", @"arrow.clockwise", self,
                                       @selector(refreshStatus:), NO);
    AddInsetRow(self.overviewGroup, self.refreshButton);

    // Footer with settings gear so settings do not dominate the panel.
    NSStackView *footerRow = [NSStackView stackViewWithViews:@[]];
    footerRow.orientation = NSUserInterfaceLayoutOrientationHorizontal;
    footerRow.distribution = NSStackViewDistributionFill;
    footerRow.alignment = NSLayoutAttributeCenterY;
    footerRow.spacing = 8;
    footerRow.edgeInsets = NSEdgeInsetsMake(8, kInnerX, 0, kInnerX);
    self.footer = MakeCaption(@"Waiting for first update");
    [self.footer setContentHuggingPriority:NSLayoutPriorityDefaultLow
                            forOrientation:NSLayoutConstraintOrientationHorizontal];
    self.settingsButton = MakeRowButton(@"", @selector(openSettings:));
    self.settingsButton.target = self;
    [self.settingsButton setImage:[NSImage imageWithSystemSymbolName:@"gearshape"
                                          accessibilityDescription:@"Settings"]];
    self.settingsButton.imagePosition = NSImageOnly;
    self.settingsButton.contentTintColor = [NSColor secondaryLabelColor];
    [self.settingsButton setAccessibilityLabel:@"Settings"];
    [self.settingsButton.widthAnchor constraintEqualToConstant:24].active = YES;
    [self.settingsButton.heightAnchor constraintEqualToConstant:24].active = YES;
    [footerRow addArrangedSubview:self.footer];
    [footerRow addArrangedSubview:self.settingsButton];
    [self.overviewGroup addArrangedSubview:footerRow];
    span(footerRow);

    // Picker group: replaces the overview body when picking.
    self.pickerGroup = [NSStackView stackViewWithViews:@[]];
    self.pickerGroup.orientation = NSUserInterfaceLayoutOrientationVertical;
    self.pickerGroup.alignment = NSLayoutAttributeLeading;
    self.pickerGroup.spacing = 4;
    self.pickerGroup.edgeInsets = NSEdgeInsetsMake(4, 0, 6, 0);
    [content addArrangedSubview:self.pickerGroup];
    span(self.pickerGroup);

    SparkyActionRow *back = MakeActionRow(@"Load model", @"chevron.left", self,
                                          @selector(backToOverview:), YES);
    AddInsetRow(self.pickerGroup, back);

    self.pickerCaption = MakeCaption(@"Run through the Sparky CLI");
    self.pickerCaption.preferredMaxLayoutWidth = kPanelWidth - 2 * kInsetX - 2 * kInnerX;
    AddInsetRow(self.pickerGroup, self.pickerCaption);

    self.choiceStack = [NSStackView stackViewWithViews:@[]];
    self.choiceStack.orientation = NSUserInterfaceLayoutOrientationVertical;
    self.choiceStack.alignment = NSLayoutAttributeLeading;
    self.choiceStack.spacing = 2;
    self.choiceStack.edgeInsets = NSEdgeInsetsMake(0, kInnerX, 0, kInnerX);
    [self.pickerGroup addArrangedSubview:self.choiceStack];
    [self.choiceStack.widthAnchor constraintEqualToAnchor:self.pickerGroup.widthAnchor].active = YES;

    [self applyPicking:NO];
}

// applyPicking shows exactly one of overview/picker. The hidden group is
// removed from layout by NSStackView, so it takes no vertical space.
- (void)applyPicking:(BOOL)picking {
    self.overviewGroup.hidden = picking;
    self.pickerGroup.hidden = !picking;
    self.view.needsLayout = YES;
}

- (void)refreshStatus:(id)sender { goRefresh(); }
- (void)toggleLoad:(id)sender { goPickerOpen(); } // Go decides open/close
- (void)backToOverview:(id)sender { goPickerClose(); }
- (void)toggleLogin:(id)sender { goToggleLogin(); }
- (void)toggleTitle:(id)sender { goToggleTitle(); }
- (void)copyDiagnostics:(id)sender { goCopyDiagnostics(); }
- (void)quitApp:(id)sender { goQuit(); }

- (void)togglePopover:(id)sender {
    if (gPopover.shown) {
        [gPopover performClose:nil];
        return;
    }
    [gPopover showRelativeToRect:gItem.button.bounds ofView:gItem.button
                  preferredEdge:NSRectEdgeMinY];
    goPanelOpened(); // on-open refresh; closing never cancels work
}

- (void)openSettings:(id)sender {
    // Rebuild the menu each time so the checkmarks reflect current state, and
    // notify Go whenever the settings menu opens so status is requested.
    NSMenu *menu = [[NSMenu alloc] initWithTitle:@"Settings"];
    NSMenuItem *login = [[NSMenuItem alloc] initWithTitle:@"Start at login"
                                                   action:@selector(toggleLogin:)
                                            keyEquivalent:@""];
    login.target = self;
    login.state = gLoginEnabled ? NSControlStateValueOn : NSControlStateValueOff;
    [menu addItem:login];
    NSMenuItem *showTitle = [[NSMenuItem alloc] initWithTitle:@"Show title"
                                                       action:@selector(toggleTitle:)
                                                keyEquivalent:@""];
    showTitle.target = self;
    showTitle.state = gShowTitle ? NSControlStateValueOn : NSControlStateValueOff;
    [menu addItem:showTitle];
    [menu addItem:[NSMenuItem separatorItem]];
    NSMenuItem *diag = [[NSMenuItem alloc] initWithTitle:@"Copy diagnostics"
                                                  action:@selector(copyDiagnostics:)
                                           keyEquivalent:@""];
    diag.target = self;
    [menu addItem:diag];
    [menu addItem:[NSMenuItem separatorItem]];
    NSMenuItem *quit = [[NSMenuItem alloc] initWithTitle:@"Quit Sparky"
                                                  action:@selector(quitApp:)
                                           keyEquivalent:@"q"];
    quit.target = self;
    [menu addItem:quit];
    goPanelOpened(); // menu opening also requests a status refresh
    [menu popUpMenuPositioningItem:nil atLocation:NSMakePoint(0, 0) inView:self.settingsButton];
}

- (void)choiceClicked:(id)sender {
    NSButton *b = (NSButton *)sender;
    NSString *modelID = b.identifier;
    // Go owns picking state; it closes the picker on accept and on refusal.
    goChoiceClick(modelID.length > 0 ? modelID.UTF8String : "");
}

@end

// MARK: - Go-facing bridge

static void OnMain(dispatch_block_t block) {
    if ([NSThread isMainThread]) block();
    else dispatch_async(dispatch_get_main_queue(), block);
}

static NSString *Str(const char *s) { return s ? [NSString stringWithUTF8String:s] : @""; }

static void ClearStack(NSStackView *stack) {
    for (NSView *v in [stack.arrangedSubviews copy]) {
        [stack removeArrangedSubview:v];
        [v removeFromSuperview];
    }
}

static SparkyPanelController *Controller(void) {
    return (SparkyPanelController *)gController;
}

// AppKit owns the root origin inside the popover chrome. Resize only its size.
static void SizePopover(void) {
    SparkyPanelController *vc = Controller();
    [vc.view layoutSubtreeIfNeeded];
    CGFloat h = ceil(vc.contentView.fittingSize.height + kInsetTop + kInsetBottom);
    [vc.view setFrameSize:NSMakeSize(kPanelWidth, MAX(1, h))];
    if (gPopover && (gPopover.contentSize.height != h || gPopover.contentSize.width != kPanelWidth)) {
        gPopover.contentSize = NSMakeSize(kPanelWidth, h);
    }
    [vc.view layoutSubtreeIfNeeded];
}

// Synchronously initialize the shared NSApplication on the caller's thread,
// which must be the process main OS thread (main_darwin.go locks it). This
// creates NSApp explicitly instead of relying on a framework side effect, and
// sets the accessory policy so no Dock icon or menu bar is added. It does not
// start the run loop; sparky_run does that on the same thread.
static void InitApplication(void) {
    @autoreleasepool {
        NSApplication *app = [NSApplication sharedApplication];
        [app setActivationPolicy:NSApplicationActivationPolicyAccessory];
    }
}

// Build the panel controller and popover. Shared by the real app bootstrap and
// the headless construction probe so both exercise identical object graphs.
// Must run on the main thread; creates no window and shows nothing.
static void BuildPanelObjects(void) {
    if (gController) return;
    gController = [[SparkyPanelController alloc] init];
    [gController loadView];
    gPopover = [[NSPopover alloc] init];
    gPopover.behavior = NSPopoverBehaviorTransient;
    gPopover.contentViewController = gController;
    SizePopover();
}

void sparky_init(void) {
    if (![NSThread isMainThread]) return; // must be the locked main OS thread
    @autoreleasepool {
        if (gItem) return;
        InitApplication();

        gItem = [[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength];
        gItem.button.imagePosition = NSImageOnly;

        BuildPanelObjects();

        // Clicking the status item toggles the popover; it never cancels work.
        gItem.button.target = gController;
        gItem.button.action = @selector(togglePopover:);

        sparky_set_icon_state(0, NULL);
    }
}

void sparky_run(void) {
    // Must run on the locked main OS thread after sparky_init. The app was
    // initialized there; run its event loop on that same thread.
    if (![NSThread isMainThread]) return;
    @autoreleasepool {
        [[NSApplication sharedApplication] run];
    }
}

void sparky_quit(void) { OnMain(^{ [[NSApplication sharedApplication] terminate:nil]; }); }

void sparky_set_icon_state(int state, const char *title) {
    NSString *t = nil;
    @autoreleasepool { t = Str(title); }
    OnMain(^{
        if (!gItem) return;
        NSString *symbol = @"bolt.fill";
        if (state == 1) symbol = @"ellipsis";
        else if (state == 2) symbol = @"exclamationmark.triangle";
        NSImage *img = [NSImage imageWithSystemSymbolName:symbol
                                   accessibilityDescription:@"Sparky"];
        img.template = YES;
        gItem.button.image = img;
        gItem.button.title = @"";
        if (t.length > 0) {
            gItem.button.imagePosition = NSImageLeft;
            gItem.button.title = t;
        } else {
            gItem.button.imagePosition = NSImageOnly;
        }
    });
}

void sparky_show_panel(void) {
    OnMain(^{
        if (!gItem.button.window) return;
        [gPopover showRelativeToRect:gItem.button.bounds ofView:gItem.button
                       preferredEdge:NSRectEdgeMinY];
        goPanelOpened();
    });
}

void sparky_hide_panel(void) { OnMain(^{ [gPopover performClose:nil]; }); }

void sparky_set_panel(const char *headerTitle, const char *headerState,
                      const char *modelCaption, const char *modelName,
                      const char *modelLocation, const char *memoryCaption,
                      const char *refreshLabel, const char *loadLabel,
                      const char *footer, const char *loadResult) {
    NSString *ht = nil, *hs = nil, *mc = nil, *mn = nil, *ml = nil;
    NSString *mm = nil, *rl = nil, *ll = nil, *ft = nil, *lr = nil;
    @autoreleasepool { // synchronous copies on the caller's thread
        ht = Str(headerTitle); hs = Str(headerState);
        mc = Str(modelCaption); mn = Str(modelName); ml = Str(modelLocation);
        mm = Str(memoryCaption); rl = Str(refreshLabel); ll = Str(loadLabel);
        ft = Str(footer); lr = Str(loadResult);
    }
    OnMain(^{
        SparkyPanelController *c = Controller();
        c.headerTitle.stringValue = ht;
        c.headerState.stringValue = hs;
        c.modelCaption.stringValue = mc;
        c.modelName.stringValue = mn;
        c.modelLocation.stringValue = ml;
        c.memoryCaption.stringValue = mm;
        [c.refreshButton setTitle:rl];
        [c.loadButton setTitle:ll];
        c.footer.stringValue = ft;
        c.loadResult.stringValue = lr;
        c.loadResult.hidden = (lr.length == 0);
        SizePopover();
    });
}

void sparky_set_extra_models(const char **lines, int count) {
    NSMutableArray *titles = nil;
    @autoreleasepool {
        titles = [NSMutableArray array];
        for (int i = 0; i < count && lines; i++) [titles addObject:Str(lines[i])];
    }
    OnMain(^{
        SparkyPanelController *c = Controller();
        ClearStack(c.extraModelsStack);
        CGFloat maxW = kPanelWidth - 2 * kInsetX - 2 * kInnerX;
        for (NSString *t in titles) {
            NSTextField *f = MakeBody(t);
            f.preferredMaxLayoutWidth = maxW;
            [c.extraModelsStack addArrangedSubview:f];
        }
        [c.view setNeedsLayout:YES];
        SizePopover();
    });
}

void sparky_set_memory_rows(const char **labels, const char **values, int count) {
    NSMutableArray *l = nil, *v = nil;
    @autoreleasepool {
        l = [NSMutableArray array];
        v = [NSMutableArray array];
        for (int i = 0; i < count && labels && values; i++) {
            [l addObject:Str(labels[i])];
            [v addObject:Str(values[i])];
        }
    }
    OnMain(^{
        SparkyPanelController *c = Controller();
        ClearStack(c.memoryStack);
        for (NSUInteger i = 0; i < l.count; i++) {
            NSTextField *right = nil;
            NSStackView *row = MakePairRow(l[i], v[i], &right);
            [c.memoryStack addArrangedSubview:row];
            [row.widthAnchor constraintEqualToAnchor:c.memoryStack.widthAnchor].active = YES;
        }
        [c.view setNeedsLayout:YES];
        SizePopover();
    });
}

void sparky_set_requests(const char *label, const char *value) {
    NSString *lab = nil, *val = nil;
    @autoreleasepool {
        lab = Str(label);
        val = Str(value);
    }
    OnMain(^{
        SparkyPanelController *c = Controller();
        ClearStack(c.requestsRow);
        NSTextField *left = MakeLabel(lab, [NSFont systemFontOfSize:13 weight:NSFontWeightRegular],
                                      [NSColor labelColor]);
        NSTextField *right = MakeValueLabel(val, 13, YES);
        [c.requestsRow addArrangedSubview:left];
        [c.requestsRow addArrangedSubview:right];
        SizePopover();
    });
}

// Picker content. Only rebuilds the choice buttons when the catalog or mode
// actually changed, so per-second renders preserve keyboard focus/selection.
void sparky_set_picker(int picking, const char *pickerCaption,
                       const char **labels, const char **ids, int count) {
    NSString *cap = nil;
    NSMutableArray *lab = nil, *idv = nil;
    @autoreleasepool {
        cap = Str(pickerCaption);
        lab = [NSMutableArray array];
        idv = [NSMutableArray array];
        for (int i = 0; i < count && labels && ids; i++) {
            [lab addObject:Str(labels[i])];
            [idv addObject:Str(ids[i])];
        }
    }
    OnMain(^{
        SparkyPanelController *c = Controller();
        if (![c.pickerCaption.stringValue isEqualToString:cap]) {
            c.pickerCaption.stringValue = cap;
        }
        BOOL sameCount = (NSUInteger)count == c.choiceStack.arrangedSubviews.count;
        BOOL sameItems = sameCount;
        if (sameItems) {
            for (NSUInteger i = 0; i < (NSUInteger)count; i++) {
                NSButton *b = c.choiceStack.arrangedSubviews[i];
                if (![b.title isEqualToString:lab[i]] || ![b.identifier isEqualToString:idv[i]]) {
                    sameItems = NO;
                    break;
                }
            }
        }
        if (!sameItems) {
            ClearStack(c.choiceStack);
            for (NSUInteger i = 0; i < lab.count; i++) {
                NSButton *b = MakeRowButton(lab[i], @selector(choiceClicked:));
                b.tag = (NSInteger)i;
                b.identifier = idv[i];
                b.target = c;
                // Add to the stack FIRST: a width constraint between the button
                // and the stack needs a common ancestor at activation time, or
                // AppKit raises NSGenericException ("no common ancestor") and
                // terminates the app during catalog delivery.
                [c.choiceStack addArrangedSubview:b];
                [b.widthAnchor constraintEqualToAnchor:c.choiceStack.widthAnchor
                                               constant:-2 * kInnerX].active = YES;
            }
        }
        [c applyPicking:(picking != 0)];
        SizePopover();
    });
}

void sparky_set_enabled(int loadEnabled, int refreshEnabled, int hasCLI) {
    OnMain(^{
        SparkyPanelController *c = Controller();
        c.loadButton.enabled = loadEnabled ? YES : NO;
        c.refreshButton.enabled = refreshEnabled ? YES : NO;
        c.loadButton.toolTip = hasCLI ? nil : @"Sparky CLI not available";
    });
}

void sparky_set_settings(int showTitle, int loginEnabled) {
    OnMain(^{
        SparkyPanelController *c = Controller();
        gShowTitle = showTitle ? YES : NO;
        gLoginEnabled = loginEnabled ? YES : NO;
        c.settingsButton.toolTip = @"Settings";
    });
}

// MARK: - Headless construction probe (test support)

// Build only the in-memory panel controller/popover object graph and report
// whether it constructed cleanly. Creates no status item, no window, shows
// nothing and never runs the NSApplication event loop.
//
// The probe does NOT initialize NSApplication and does NOT dispatch to the main
// queue: the native test runs it synchronously on the locked process main
// thread, where OnMain's isMainThread branch executes inline. Returns 0 on
// success, 1 on an AppKit exception, 2 when not on the main thread.
int sparky_probe_build_panel(void) {
    if (![NSThread isMainThread]) return 2;
    @try {
        @autoreleasepool { BuildPanelObjects(); }
        return 0;
    } @catch (NSException *ex) {
        NSLog(@"sparky panel construction probe raised %@: %@", ex.name, ex.reason);
        return 1;
    }
}

// Report the number of choice rows currently arranged in the picker stack, so
// the regression can prove the real picker path rebuilt rows.
int sparky_probe_choice_count(void) {
    if (![NSThread isMainThread] || !gController) return -1;
    @try {
        return (int)Controller().choiceStack.arrangedSubviews.count;
    } @catch (NSException *ex) {
        NSLog(@"sparky panel probe choice count raised %@: %@", ex.name, ex.reason);
        return -1;
    }
}

// Report the picker caption the view is showing as a caller-owned strdup'd
// copy (freed by the Go wrapper), so cgo never retains an AppKit pointer.
char *sparky_probe_picker_caption(void) {
    if (![NSThread isMainThread] || !gController) return NULL;
    @try {
        NSString *s = Controller().pickerCaption.stringValue ?: @"";
        return strdup(s.UTF8String ?: "");
    } @catch (NSException *ex) {
        NSLog(@"sparky panel probe caption raised %@: %@", ex.name, ex.reason);
        return NULL;
    }
}

static NSRect AlignedRect(NSView *view, NSView *root) {
    return [view.superview convertRect:[view alignmentRectForFrame:view.frame] toView:root];
}

// Exercise actual layout without a window. The synthetic host models the
// nonzero content origin assigned by NSPopover, which must survive refreshes.
char *sparky_probe_layout(int picking) {
    if (![NSThread isMainThread] || !gController) return strdup("layout probe requires main thread");
    SparkyPanelController *c = Controller();
    NSView *root = c.view;
    NSView *previous = root.superview;
    NSRect previousFrame = root.frame;
    NSView *host = [[NSView alloc] initWithFrame:NSMakeRect(0, 0, 600, 800)];
    @try {
        [host addSubview:root];
        [root setFrameOrigin:NSMakePoint(13, 17)];
        SizePopover();
        if (!NSEqualPoints(root.frame.origin, NSMakePoint(13, 17)))
            return strdup("resize changed the popover content origin");
        CGFloat left = kInsetX + kInnerX, right = kPanelWidth - left;
        NSArray *labels = picking ? @[c.pickerCaption] : @[c.headerTitle, c.modelName, c.memoryCaption, c.footer];
        for (NSView *view in labels) {
            NSRect r = AlignedRect(view, root);
            if (fabs(NSMinX(r) - left) > 0.5 || NSMaxX(r) > right + 0.5)
                return strdup([[NSString stringWithFormat:@"text outside column: %@", NSStringFromRect(r)] UTF8String]);
        }
        if (picking) {
            for (NSView *view in c.choiceStack.arrangedSubviews) {
                NSRect r = AlignedRect(view, root);
                if (fabs(NSMinX(r) - left) > 0.5 || fabs(NSMaxX(r) - right) > 0.5)
                    return strdup("picker choice exceeds its inset column");
            }
        } else {
            NSRect state = AlignedRect(c.headerState, root);
            NSRect load = AlignedRect(c.loadButton, root);
            if (fabs(NSMaxX(state) - right) > 0.5 || fabs(NSMinX(load) - left) > 0.5 ||
                fabs(NSMaxX(load) - right) > 0.5)
                return strdup("header state or action row is outside its column");
            if (c.loadButton.leadingGlyph || !c.loadButton.trailingGlyph)
                return strdup("Load must have a trailing chevron");
        }
        CGFloat expected = ceil(c.contentView.fittingSize.height + kInsetTop + kInsetBottom);
        if (fabs(NSHeight(root.frame) - expected) > 0.5)
            return strdup("panel has surplus height beyond its content");
        return NULL;
    } @catch (NSException *ex) {
        return strdup(ex.reason.UTF8String);
    } @finally {
        [root removeFromSuperview];
        if (previous) [previous addSubview:root];
        root.frame = previousFrame;
    }
}
