# Choosing

Short decision guides for the questions every new user of the library asks. Each one has a comparison table, an "if this, then that" list, and for every option a link to the guide that teaches it and the program that runs it. The capability claims are read from each driver's `Capabilities()` method; the full tables are in the [capabilities reference](../reference/capabilities.md).

| Question | Guide | Read it when |
|---|---|---|
| Which backend should hold my files? | [Which adapter](which-adapter.md) | before you write a DSN, or when a feature needs a capability your backend lacks |
| How should bytes get from the client to storage? | [Which upload method](which-upload-method.md) | before you build an upload endpoint or pick a client library |
| How should files get back to the user? | [Serving files](serving-files.md) | before you hand out the first link |
| Do I need files as rows in my database, and which plugins? | [Do I need records?](do-i-need-records.md) | when a file starts to belong to a user, a product or a case |
| What does an app like mine use? | [Plugins by app](plugins-by-app.md) | when you start a SaaS, social, e-commerce, document, media or chat product |

New to the library? The [learning path](../README.md#learning-path) takes you through every piece in order.
