export namespace persona {
	
	export class Persona {
	    id: string;
	    name: string;
	    seedText: string;
	    origin: string;
	    isBuiltin: boolean;
	    avatarPath: string;
	    createdAt: number;
	    updatedAt: number;
	
	    static createFrom(source: any = {}) {
	        return new Persona(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.seedText = source["seedText"];
	        this.origin = source["origin"];
	        this.isBuiltin = source["isBuiltin"];
	        this.avatarPath = source["avatarPath"];
	        this.createdAt = source["createdAt"];
	        this.updatedAt = source["updatedAt"];
	    }
	}
	export class PersonaChange {
	    id: string;
	    personaId: string;
	    ruleId: string;
	    action: string;
	    field: string;
	    oldValue: string;
	    newValue: string;
	    source: string;
	    evidence: string;
	    createdAt: number;
	
	    static createFrom(source: any = {}) {
	        return new PersonaChange(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.personaId = source["personaId"];
	        this.ruleId = source["ruleId"];
	        this.action = source["action"];
	        this.field = source["field"];
	        this.oldValue = source["oldValue"];
	        this.newValue = source["newValue"];
	        this.source = source["source"];
	        this.evidence = source["evidence"];
	        this.createdAt = source["createdAt"];
	    }
	}
	export class PersonaRule {
	    id: string;
	    personaId: string;
	    slot: string;
	    value: string;
	    source: string;
	    evidence: string;
	    tier: string;
	    kind: string;
	    priority: number;
	    enabled: boolean;
	    createdAt: number;
	    updatedAt: number;
	
	    static createFrom(source: any = {}) {
	        return new PersonaRule(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.personaId = source["personaId"];
	        this.slot = source["slot"];
	        this.value = source["value"];
	        this.source = source["source"];
	        this.evidence = source["evidence"];
	        this.tier = source["tier"];
	        this.kind = source["kind"];
	        this.priority = source["priority"];
	        this.enabled = source["enabled"];
	        this.createdAt = source["createdAt"];
	        this.updatedAt = source["updatedAt"];
	    }
	}
	export class Snapshot {
	    personas: Persona[];
	    activeId: string;
	    rules: PersonaRule[];
	    recentChanges: PersonaChange[];
	    storageReady: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Snapshot(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.personas = this.convertValues(source["personas"], Persona);
	        this.activeId = source["activeId"];
	        this.rules = this.convertValues(source["rules"], PersonaRule);
	        this.recentChanges = this.convertValues(source["recentChanges"], PersonaChange);
	        this.storageReady = source["storageReady"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace ui {
	
	export class HistoryItem {
	    id: string;
	    role: string;
	    text: string;
	    at: number;
	    status: string;
	
	    static createFrom(source: any = {}) {
	        return new HistoryItem(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.role = source["role"];
	        this.text = source["text"];
	        this.at = source["at"];
	        this.status = source["status"];
	    }
	}

}

